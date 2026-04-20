package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// paths builds per-test stdout+stderr files under t.TempDir so tests
// stay parallel-safe and don't pollute each other.
func paths(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	return filepath.Join(d, "stdout"), filepath.Join(d, "stderr")
}

func TestRunSuccess(t *testing.T) {
	out, errf := paths(t)
	resp, err := Run(context.Background(), RunRequest{
		Argv:       []string{"sh", "-c", "printf hello; printf err >&2; exit 0"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", resp.ExitCode)
	}
	if resp.Retries != 0 {
		t.Errorf("Retries = %d, want 0", resp.Retries)
	}
	if resp.RateLimited {
		t.Errorf("RateLimited = true, want false")
	}
	gotOut, _ := os.ReadFile(out)
	if string(gotOut) != "hello" {
		t.Errorf("stdout = %q, want %q", gotOut, "hello")
	}
	if _, statErr := os.Stat(errf); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stderr file should be deleted on success, got stat err = %v", statErr)
	}
}

func TestRunPipesPromptToStdin(t *testing.T) {
	out, errf := paths(t)
	resp, err := Run(context.Background(), RunRequest{
		// `cat` echoes whatever it receives on stdin
		Argv:       []string{"cat"},
		Prompt:     "the prompt body\n",
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", resp.ExitCode)
	}
	gotOut, _ := os.ReadFile(out)
	if string(gotOut) != "the prompt body\n" {
		t.Errorf("stdout = %q, want %q", gotOut, "the prompt body\n")
	}
}

func TestRunPassesEnv(t *testing.T) {
	out, errf := paths(t)
	resp, err := Run(context.Background(), RunRequest{
		Argv:       []string{"sh", "-c", "printf %s \"$COUNCIL_TEST_VAR\""},
		Env:        []string{"COUNCIL_TEST_VAR=visible"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", resp.ExitCode)
	}
	gotOut, _ := os.ReadFile(out)
	if string(gotOut) != "visible" {
		t.Errorf("env not passed: stdout = %q", gotOut)
	}
}

func TestRunTimeoutKillsAndReturnsErrTimeout(t *testing.T) {
	out, errf := paths(t)
	start := time.Now()
	resp, err := Run(context.Background(), RunRequest{
		Argv:       []string{"sh", "-c", "sleep 30"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    300 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run took %s — process-group kill may not have fired", elapsed)
	}
	if resp.Retries != 0 {
		t.Errorf("Retries = %d, want 0 (MaxRetries default = 0)", resp.Retries)
	}
}

func TestRunNonZeroExitWithoutRateLimit(t *testing.T) {
	out, errf := paths(t)
	resp, err := Run(context.Background(), RunRequest{
		Argv:       []string{"sh", "-c", "echo just an error >&2; exit 7"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    5 * time.Second,
	})
	if !errors.Is(err, ErrNonZeroExit) {
		t.Fatalf("err = %v, want ErrNonZeroExit", err)
	}
	if resp.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", resp.ExitCode)
	}
	if resp.Retries != 0 {
		t.Errorf("Retries = %d, want 0", resp.Retries)
	}
	if resp.RateLimited {
		t.Errorf("RateLimited = true, want false")
	}
	// stderr file should still exist on failure (design §7)
	if _, err := os.Stat(errf); err != nil {
		t.Errorf("stderr file should be persisted on failure: %v", err)
	}
}

func TestRunFailRetriesUseMaxRetriesBudget(t *testing.T) {
	out, errf := paths(t)
	resp, err := Run(context.Background(), RunRequest{
		Argv:       []string{"sh", "-c", "echo nope >&2; exit 1"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    5 * time.Second,
		MaxRetries: 2,
	})
	if !errors.Is(err, ErrNonZeroExit) {
		t.Fatalf("err = %v, want ErrNonZeroExit", err)
	}
	if resp.Retries != 2 {
		t.Errorf("Retries = %d, want 2 (MaxRetries=2 → 3 total attempts → 2 retries)", resp.Retries)
	}
}

// TestRunCtxCancelSkipsFailRetry asserts that a parent-context cancellation
// short-circuits the fail-retry loop even with a generous MaxRetries.
// Retrying a cancelled subprocess just respawns a child that will be killed
// again by the same context — so the loop must treat context.Canceled /
// DeadlineExceeded as terminal.
func TestRunCtxCancelSkipsFailRetry(t *testing.T) {
	out, errf := paths(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := Run(ctx, RunRequest{
		Argv:       []string{"sh", "-c", "sleep 30"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    5 * time.Second,
		MaxRetries: 5,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if resp.Retries != 0 {
		t.Errorf("Retries = %d, want 0 (cancellation must not consume retry budget)", resp.Retries)
	}
}

func TestRunRateLimitRetriesUsesRateLimitBudget(t *testing.T) {
	// Per design §10: rate-limit retries are runner-owned with budget
	// req.RateLimitMaxRetries (callers materialise the design's
	// "max_retries+1" by passing profile.MaxRetries+1 — see
	// pkg/executor/claudecode). Here we set it to 1 directly.
	out, errf := paths(t)
	start := time.Now()
	resp, err := Run(context.Background(), RunRequest{
		Argv: []string{"sh", "-c", `printf "rate_limit exceeded\nRetry-After: 1s\n" >&2; exit 1`},
		// override default 10s sleep with a 1s Retry-After hint so the
		// test runs quickly
		StdoutFile:          out,
		StderrFile:          errf,
		Timeout:             5 * time.Second,
		MaxRetries:          0,
		RateLimitMaxRetries: 1,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if !resp.RateLimited {
		t.Errorf("RateLimited = false, want true")
	}
	if resp.Retries != 1 {
		t.Errorf("Retries = %d, want 1 (RateLimitMaxRetries=1)", resp.Retries)
	}
	if elapsed < 800*time.Millisecond {
		t.Errorf("elapsed = %s — Retry-After hint was probably not honored", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("elapsed = %s — Retry-After hint was probably ignored (used 10s default)", elapsed)
	}
}

// TestRunRateLimitNoRetryWithZeroBudget locks in that a zero
// RateLimitMaxRetries means "no rate-limit retries" — the runner returns
// ErrRateLimit on the first 429 instead of silently allowing one freebie.
// This is the behavior change from the prior (req.MaxRetries+1) bound,
// which always granted one retry regardless of caller intent.
func TestRunRateLimitNoRetryWithZeroBudget(t *testing.T) {
	out, errf := paths(t)
	resp, err := Run(context.Background(), RunRequest{
		Argv:                []string{"sh", "-c", `printf "rate_limit exceeded\n" >&2; exit 1`},
		StdoutFile:          out,
		StderrFile:          errf,
		Timeout:             5 * time.Second,
		MaxRetries:          0,
		RateLimitMaxRetries: 0,
	})
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if resp.Retries != 0 {
		t.Errorf("Retries = %d, want 0 (zero rate-limit budget)", resp.Retries)
	}
}

func TestRunCallerCancelReturnsCtxErrNotTimeout(t *testing.T) {
	out, errf := paths(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := Run(ctx, RunRequest{
		Argv:       []string{"sh", "-c", "sleep 30"},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    10 * time.Second,
	})
	// caller cancellation must surface as context.Canceled, not
	// ErrTimeout — orchestrator distinguishes "user hit Ctrl-C" from
	// "expert ran past its budget"
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunProcessGroupKillReapsGrandchildren(t *testing.T) {
	// Spawn a shell that backgrounds two `sleep 30` children, records
	// their PIDs to a file, then waits. After the timeout fires we
	// verify both backgrounded children are dead — the process-group
	// kill (signal -pgid) should have reaped them, not just the parent
	// shell.
	tmp := t.TempDir()
	pidsFile := filepath.Join(tmp, "pids")
	out, errf := paths(t)
	script := `sleep 30 &
echo $! >> ` + pidsFile + `
sleep 30 &
echo $! >> ` + pidsFile + `
wait
`
	_, err := Run(context.Background(), RunRequest{
		Argv:       []string{"sh", "-c", script},
		StdoutFile: out,
		StderrFile: errf,
		Timeout:    300 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	// Give the OS a beat to deliver SIGKILL and reap.
	time.Sleep(150 * time.Millisecond)
	pidBytes, readErr := os.ReadFile(pidsFile)
	if readErr != nil {
		t.Fatalf("read pids file: %v", readErr)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(pidBytes)), "\n") {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
		if parseErr != nil || pid <= 0 {
			t.Errorf("bad pid line %q: %v", line, parseErr)
			continue
		}
		// signal 0 is "check existence". ESRCH means dead.
		if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
			t.Errorf("pid %d still alive after process-group kill", pid)
			// best-effort cleanup so we don't leave stragglers
			_ = syscall.Kill(pid, syscall.SIGKILL)
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Errorf("pid %d: kill(0) = %v, want ESRCH", pid, err)
		}
	}
}
