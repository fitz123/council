// Package runner is the subprocess primitive used by every executor. It
// owns the low-level concerns common to every CLI we shell out to:
//
//   - process-group spawn + kill (so a hung child plus any grandchildren
//     die together on timeout, rather than leaking as orphans),
//   - stdin piping of the prompt body,
//   - stdout/stderr capture into caller-named files,
//   - timeout enforcement (SIGTERM, 2s grace, SIGKILL),
//   - fail-retry policy (timeout, non-zero exit) bounded by req.MaxRetries.
//
// Per ADR-0013 (no runner-side rate-limit retries), rate-limit detection
// has moved out of this package. Executors call DetectLimit themselves
// against their own per-vendor marker list and wrap the runner error into
// *LimitError so the orchestrator (pkg/debate) can classify it via
// errors.As. The runner no longer reads stderr for any classification
// purpose; it just reports the exit code and surfaces ErrNonZeroExit.
//
// Fail-retries (timeout, non-zero exit) remain runner-owned via MaxRetries.
// pkg/executor/claudecode passes MaxRetries: 0 so pkg/debate stays in
// charge of fail-retry policy.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// RunRequest describes a single subprocess invocation. Argv[0] is the
// program; Argv[1:] are arguments. Prompt is piped to the child's stdin
// in full (not streamed line-by-line), then stdin is closed.
//
// Env is passed through to exec.Cmd.Env when non-nil, so callers can set
// things like CLAUDE_CODE_MAX_OUTPUT_TOKENS without reaching into runner
// internals. nil Env means inherit the parent process environment.
//
// MaxRetries is the per-call fail-retry budget (timeout, non-zero exit).
// Rate-limit detection and any related back-off happens at the executor
// layer (ADR-0013), not here.
type RunRequest struct {
	Argv       []string
	Prompt     string
	Env        []string
	StdoutFile string
	StderrFile string
	Timeout    time.Duration
	MaxRetries int
}

// RunResponse summarizes the final attempt. Retries counts every retry
// across the fail-retry path so the orchestrator can record a single
// number into verdict.json.
//
// ExitCode carries the final attempt's exit code, with one sentinel:
// KilledExitCode (-1) means the subprocess was killed before it could
// exit (timeout or parent-ctx cancellation). Callers that persist
// verdict.json can surface -1 directly so readers can distinguish
// "exited cleanly with code 0" from "never got to exit".
type RunResponse struct {
	ExitCode int
	Duration time.Duration
	Retries  int
}

// KilledExitCode is the ExitCode sentinel returned when a subprocess was
// killed by the runner (timeout) or via parent-ctx cancellation, i.e. it
// never reached a clean exit. Using -1 keeps it distinct from any real
// process exit code (0–255 on POSIX).
const KilledExitCode = -1

// Sentinel errors. Callers should use errors.Is for matching, since the
// error wrapping path includes context (which attempt failed, exit code,
// etc.) that future versions may extend.
var (
	ErrTimeout     = errors.New("runner: subprocess timed out")
	ErrNonZeroExit = errors.New("runner: subprocess exited non-zero")
)

// Run executes req per the policy in the package doc. It blocks until
// the final attempt resolves. The returned RunResponse always reflects
// the final attempt's exit code and the cumulative retry count, even on
// error — this lets callers persist attempt metadata in verdict.json
// without a second bookkeeping path.
//
// ctx cancellation is honored both during the active subprocess (the
// process group is killed) and during inter-attempt sleep. Cancellation
// returns ctx.Err() (not ErrTimeout) so callers can tell "caller asked
// us to stop" apart from "we hit our own deadline".
func Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	var resp RunResponse
	var failRetries int
	for {
		exitCode, dur, err := runOnce(ctx, req)
		resp.Duration += dur
		resp.ExitCode = exitCode
		if err == nil {
			// success — drop the stderr file (design §7: persisted only on failure)
			if req.StderrFile != "" {
				_ = os.Remove(req.StderrFile)
			}
			return resp, nil
		}
		// ErrTimeout or ErrNonZeroExit — or a parent-ctx cancellation that
		// surfaced as killReason above. Context cancellation must not
		// consume retry budget: retrying a cancelled ctx just respawns a
		// subprocess that will be killed again.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resp, err
		}
		if failRetries >= req.MaxRetries {
			return resp, err
		}
		failRetries++
		resp.Retries++
	}
}

// runOnce performs one attempt and returns its raw outcome. The caller
// (Run) is responsible for retry decisions.
func runOnce(parent context.Context, req RunRequest) (exitCode int, dur time.Duration, err error) {
	if len(req.Argv) == 0 {
		return 0, 0, fmt.Errorf("runner: empty Argv")
	}
	if req.StdoutFile == "" {
		return 0, 0, fmt.Errorf("runner: StdoutFile is required")
	}
	if req.StderrFile == "" {
		return 0, 0, fmt.Errorf("runner: StderrFile is required")
	}
	if req.Timeout <= 0 {
		return 0, 0, fmt.Errorf("runner: Timeout must be > 0, got %s", req.Timeout)
	}

	stdout, openErr := os.Create(req.StdoutFile)
	if openErr != nil {
		return 0, 0, fmt.Errorf("runner: open stdout %s: %w", req.StdoutFile, openErr)
	}
	defer stdout.Close()

	stderr, openErr := os.Create(req.StderrFile)
	if openErr != nil {
		// Remove the empty stdout file so a failed stderr-open doesn't
		// leave an orphan zero-byte output.md behind (which would confuse
		// offline readers looking for a session's artifacts).
		_ = stdout.Close()
		_ = os.Remove(req.StdoutFile)
		return 0, 0, fmt.Errorf("runner: open stderr %s: %w", req.StderrFile, openErr)
	}
	defer stderr.Close()

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if req.Env != nil {
		cmd.Env = req.Env
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, time.Since(start), fmt.Errorf("runner: start: %w", err)
	}

	// Watch process exit on a separate goroutine so we can multiplex
	// against the timeout and the parent context.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()

	var waitErr error
	var killed bool
	var killReason error // ErrTimeout if our deadline; parent ctx.Err() if caller cancelled
	select {
	case waitErr = <-waitCh:
		// natural exit
	case <-timer.C:
		killed = true
		killReason = ErrTimeout
		waitErr = killProcessGroup(cmd, waitCh)
	case <-parent.Done():
		killed = true
		killReason = parent.Err()
		waitErr = killProcessGroup(cmd, waitCh)
	}
	_ = waitErr // exit error after a forced kill is uninformative; killReason carries the signal
	dur = time.Since(start)

	// Flush captured output to disk so executors that call DetectLimit
	// after Run returns see a complete stderr file.
	_ = stdout.Sync()
	_ = stderr.Sync()

	if killed {
		// KilledExitCode (-1) distinguishes "runner tore the subprocess
		// down" from "subprocess exited cleanly with code 0" — readers
		// of verdict.json need to see the difference.
		return KilledExitCode, dur, killReason
	}

	if waitErr == nil {
		return 0, dur, nil
	}

	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else {
		// non-ExitError wait failure (e.g. wait syscall error). Treat
		// as a runtime error so callers can distinguish from "child
		// ran and exited non-zero".
		return 0, dur, fmt.Errorf("runner: wait: %w", waitErr)
	}
	return exitCode, dur, fmt.Errorf("%w (exit %d)", ErrNonZeroExit, exitCode)
}

// killProcessGroup signals the spawned process's group with SIGTERM,
// waits up to 2 seconds for the child to exit, then escalates to
// SIGKILL. It drains waitCh so the caller does not need to perform a
// second read; the returned error is whatever cmd.Wait reported (the
// caller does not use it because killReason in runOnce already carries
// the signal that caused the kill).
//
// Using Getpgid rather than -cmd.Process.Pid matters: if the child has
// called its own setpgid, the bare-PID negation targets the wrong group
// and our signal goes to the parent (us) or nowhere.
func killProcessGroup(cmd *exec.Cmd, waitCh <-chan error) error {
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid <= 0 {
		// fall back to direct PID — better than no signal at all
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case e := <-waitCh:
			return e
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			return <-waitCh
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case e := <-waitCh:
		return e
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return <-waitCh
	}
}
