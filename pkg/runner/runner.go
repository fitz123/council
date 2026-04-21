// Package runner is the subprocess primitive used by every executor. It
// owns the low-level concerns common to every CLI we shell out to:
//
//   - process-group spawn + kill (so a hung child plus any grandchildren
//     die together on timeout, rather than leaking as orphans),
//   - stdin piping of the prompt body,
//   - stdout/stderr capture into caller-named files,
//   - timeout enforcement (SIGTERM, 2s grace, SIGKILL),
//   - rate-limit (429) detection from the captured stderr,
//   - retry policy.
//
// Retry-ownership split (load-bearing — see docs/design/v1.md §10):
//
//   - Rate-limit retries are RUNNER-OWNED. They are an infrastructure
//     concern: every CLI we wrap inherits the same back-off behavior, and
//     a 429 should not consume the caller's policy budget. Bound: up to
//     req.RateLimitMaxRetries rate-limit retries (the design's
//     "max_retries+1" wording is materialised at the call site by passing
//     profile.MaxRetries+1 here — see pkg/executor/claudecode).
//   - Fail-retries (timeout, non-zero exit without a rate-limit marker)
//     are ORCHESTRATOR-OWNED. They are a policy concern driven by the
//     profile's max_retries field. Callers that want to keep all
//     fail-retry decisions at the orchestrator layer pass MaxRetries: 0
//     to disable runner-side fail-retry; the orchestrator then re-invokes
//     Run once per fail-retry it wants to grant.
//
// pkg/executor/claudecode follows the second convention: it passes
// MaxRetries: 0 so pkg/orchestrator stays in charge of fail-retry policy,
// and sets RateLimitMaxRetries: profile.MaxRetries+1 so the rate-limit
// budget tracks the profile.
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
// MaxRetries is the per-call fail-retry budget (timeout, non-zero exit
// without a rate-limit marker). RateLimitMaxRetries is the per-call
// rate-limit retry budget (429s); see the package doc for the
// runner-owned vs orchestrator-owned split.
type RunRequest struct {
	Argv                []string
	Prompt              string
	Env                 []string
	StdoutFile          string
	StderrFile          string
	Timeout             time.Duration
	MaxRetries          int
	RateLimitMaxRetries int
}

// RunResponse summarizes the final attempt. Retries counts every retry
// across both rate-limit and fail-retry paths, so the orchestrator can
// record a single number into verdict.json regardless of which kind of
// retry happened. RateLimited is sticky: it is true if any attempt
// observed a 429 marker, even if the final attempt did not.
//
// ExitCode carries the final attempt's exit code, with one sentinel:
// KilledExitCode (-1) means the subprocess was killed before it could
// exit (timeout or parent-ctx cancellation). Callers that persist
// verdict.json can surface -1 directly so readers can distinguish
// "exited cleanly with code 0" from "never got to exit".
type RunResponse struct {
	ExitCode    int
	Duration    time.Duration
	Retries     int
	RateLimited bool
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
	ErrRateLimit   = errors.New("runner: rate-limited (429) by upstream")
)

// Run executes req per the policy in the package doc. It blocks until
// the final attempt resolves. The returned RunResponse always reflects
// the final attempt's exit code and the cumulative retry count, even on
// error — this lets callers persist attempt metadata in verdict.json
// without a second bookkeeping path.
//
// ctx cancellation is honored both during the active subprocess (the
// process group is killed) and during inter-attempt rate-limit sleep.
// Cancellation returns ctx.Err() (not ErrTimeout) so callers can tell
// "caller asked us to stop" apart from "we hit our own deadline".
func Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	var resp RunResponse
	var failRetries, rlRetries int
	for {
		exitCode, retryAfter, dur, err := runOnce(ctx, req)
		resp.Duration += dur
		resp.ExitCode = exitCode
		if err == nil {
			// success — drop the stderr file (design §7: persisted only on failure)
			if req.StderrFile != "" {
				_ = os.Remove(req.StderrFile)
			}
			return resp, nil
		}
		if errors.Is(err, ErrRateLimit) {
			resp.RateLimited = true
			if rlRetries >= req.RateLimitMaxRetries {
				return resp, err
			}
			rlRetries++
			resp.Retries++
			wait := retryAfter
			if wait <= 0 {
				wait = 10 * time.Second
			}
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return resp, ctx.Err()
			}
			continue
		}
		// ErrTimeout or ErrNonZeroExit (no rate-limit marker) — or a
		// parent-ctx cancellation that surfaced as killReason above.
		// Context cancellation must not consume retry budget: retrying a
		// cancelled ctx just respawns a subprocess that will be killed
		// again.
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
//
// retryAfter is non-zero only when the attempt was classified as
// rate-limited AND the stderr included a parseable Retry-After hint;
// otherwise the caller falls back to a default sleep.
func runOnce(parent context.Context, req RunRequest) (exitCode int, retryAfter time.Duration, dur time.Duration, err error) {
	if len(req.Argv) == 0 {
		return 0, 0, 0, fmt.Errorf("runner: empty Argv")
	}
	if req.StdoutFile == "" {
		return 0, 0, 0, fmt.Errorf("runner: StdoutFile is required")
	}
	if req.StderrFile == "" {
		return 0, 0, 0, fmt.Errorf("runner: StderrFile is required")
	}
	if req.Timeout <= 0 {
		return 0, 0, 0, fmt.Errorf("runner: Timeout must be > 0, got %s", req.Timeout)
	}

	stdout, openErr := os.Create(req.StdoutFile)
	if openErr != nil {
		return 0, 0, 0, fmt.Errorf("runner: open stdout %s: %w", req.StdoutFile, openErr)
	}
	defer stdout.Close()

	stderr, openErr := os.Create(req.StderrFile)
	if openErr != nil {
		// Remove the empty stdout file so a failed stderr-open doesn't
		// leave an orphan zero-byte output.md behind (which would confuse
		// offline readers looking for a session's artifacts).
		_ = stdout.Close()
		_ = os.Remove(req.StdoutFile)
		return 0, 0, 0, fmt.Errorf("runner: open stderr %s: %w", req.StderrFile, openErr)
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
		return 0, 0, time.Since(start), fmt.Errorf("runner: start: %w", err)
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

	// Flush captured output to disk before classification — the
	// rate-limit scan reads the stderr file.
	_ = stdout.Sync()
	_ = stderr.Sync()

	if killed {
		// KilledExitCode (-1) distinguishes "runner tore the subprocess
		// down" from "subprocess exited cleanly with code 0" — readers
		// of verdict.json need to see the difference.
		return KilledExitCode, 0, dur, killReason
	}

	if waitErr == nil {
		return 0, 0, dur, nil
	}

	// non-zero exit. Capture exit code, then probe stderr for 429.
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else {
		// non-ExitError wait failure (e.g. wait syscall error). Treat
		// as a runtime error so callers can distinguish from "child
		// ran and exited non-zero".
		return 0, 0, dur, fmt.Errorf("runner: wait: %w", waitErr)
	}
	if hint, ok := scanStderr(req.StderrFile); ok {
		return exitCode, hint, dur, ErrRateLimit
	}
	return exitCode, 0, dur, fmt.Errorf("%w (exit %d)", ErrNonZeroExit, exitCode)
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
