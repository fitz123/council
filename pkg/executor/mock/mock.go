//go:build testbinary

// Package mock provides stub Executor implementations for the smoke
// test suite. It is gated behind the `testbinary` build tag so the
// release binary cannot accidentally include it — the tag is present
// only when smoke tests build a parallel `council-test` binary via
// `go build -tags testbinary -o council-test ./cmd/council`.
//
// All stubs register themselves as the single executor named
// "claude-code" so the default profile YAML (which references that
// executor) routes to mock behavior with no profile changes. Behavior
// is selected at runtime via the COUNCIL_MOCK_EXECUTOR env var:
//
//   - "" or "trivial"        — write a fixed string to stdout, succeed
//   - "slow"                 — block on ctx.Done(), return its error
//   - "fail_once_then_ok"    — first Execute per StdoutFile path fails;
//     subsequent calls succeed (lets F4 verify
//     the orchestrator-level retry path)
//   - "echo-stdin-length"    — write `[stdin-bytes=<N>]` where N is the
//     full Prompt byte count (lets F6 verify
//     the prompt actually reached the child)
//
// State for "fail_once_then_ok" is keyed by Request.StdoutFile, which
// is unique per (session, expert) because the orchestrator allocates a
// distinct directory per stage. This keeps each expert in a single
// session getting exactly one fail before its retry succeeds.
package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fitz123/council/pkg/executor"
)

// EnvName is the runtime selector for stub behavior. Exported so tests
// in this package and in test/smoke can set/clear it without
// duplicating the literal.
const EnvName = "COUNCIL_MOCK_EXECUTOR"

// init registers Mock under the "claude-code" name so the default
// profile YAML routes to the stub when this package is linked in.
func init() {
	executor.Register(&Mock{})
}

// Mock dispatches each Execute call to one of the named behaviors. It
// holds no per-call state directly; the only persistent state is the
// failOnce map used by the "fail_once_then_ok" behavior.
type Mock struct{}

// Name returns the registry key. Matches the real ClaudeCode name so
// profiles do not need editing for smoke runs.
func (*Mock) Name() string { return "claude-code" }

// Execute reads COUNCIL_MOCK_EXECUTOR and dispatches. An unknown value
// is a programmer error in the smoke test setup, not user input, so we
// return an error rather than silently defaulting.
func (*Mock) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	behavior := os.Getenv(EnvName)
	if behavior == "" {
		behavior = "trivial"
	}
	switch behavior {
	case "trivial":
		return doTrivial(ctx, req)
	case "slow":
		return doSlow(ctx, req)
	case "fail_once_then_ok":
		return doFailOnceThenOk(ctx, req)
	case "echo-stdin-length":
		return doEchoStdinLength(ctx, req)
	default:
		return executor.Response{}, fmt.Errorf("mock: unknown %s=%q", EnvName, behavior)
	}
}

// doTrivial writes a fixed body to the stdout file and returns
// success. Used by F1's deterministic counterparts (F2, F3, F7).
func doTrivial(_ context.Context, req executor.Request) (executor.Response, error) {
	start := time.Now()
	if err := os.WriteFile(req.StdoutFile, []byte("trivial mock answer\n"), 0o644); err != nil {
		return executor.Response{}, fmt.Errorf("mock trivial: write stdout: %w", err)
	}
	return executor.Response{ExitCode: 0, Duration: time.Since(start)}, nil
}

// doSlow blocks until the parent context is cancelled, then returns
// ctx.Err(). The orchestrator's runWithFailRetry recognizes that as a
// cancellation (not a fail-retry condition), so the per-expert status
// becomes "interrupted" and the top-level verdict status is
// "interrupted" — the F5 contract.
func doSlow(ctx context.Context, _ executor.Request) (executor.Response, error) {
	start := time.Now()
	<-ctx.Done()
	return executor.Response{ExitCode: 0, Duration: time.Since(start)}, ctx.Err()
}

// failOnceState tracks which StdoutFile paths have already seen one
// failed attempt. Keyed by string (the path) — sync.Map handles the
// concurrent fan-out across expert goroutines without a mutex.
var failOnceState sync.Map

// doFailOnceThenOk fails the first Execute call for any given
// StdoutFile path, then succeeds for every subsequent call. The
// orchestrator's retry loop (max_retries=1 in the default profile)
// turns this into one fail + one success per expert, so verdict.json
// records retries=1 — exactly what F4 asserts.
func doFailOnceThenOk(_ context.Context, req executor.Request) (executor.Response, error) {
	start := time.Now()
	if _, hadFailed := failOnceState.LoadOrStore(req.StdoutFile, true); !hadFailed {
		_ = os.WriteFile(req.StderrFile, []byte("mock fail_once_then_ok: first attempt fails\n"), 0o644)
		return executor.Response{ExitCode: 1, Duration: time.Since(start)},
			errors.New("mock fail_once_then_ok: first attempt fails by design")
	}
	if err := os.WriteFile(req.StdoutFile, []byte("succeeded after retry\n"), 0o644); err != nil {
		return executor.Response{}, fmt.Errorf("mock fail_once_then_ok: write stdout: %w", err)
	}
	return executor.Response{ExitCode: 0, Duration: time.Since(start)}, nil
}

// doEchoStdinLength writes `[stdin-bytes=<N>]` where N is len(Prompt).
// F6 uses this to prove the question bytes actually reached the
// subprocess, not just question.md on disk. The Prompt the executor
// receives is the BUILT expert prompt (role body + delimiters +
// question), so the byte count is larger than the raw question — the
// F6 test computes the expected value rather than hard-coding it.
func doEchoStdinLength(_ context.Context, req executor.Request) (executor.Response, error) {
	start := time.Now()
	out := fmt.Sprintf("[stdin-bytes=%d]\n", len(req.Prompt))
	if err := os.WriteFile(req.StdoutFile, []byte(out), 0o644); err != nil {
		return executor.Response{}, fmt.Errorf("mock echo-stdin-length: write stdout: %w", err)
	}
	return executor.Response{ExitCode: 0, Duration: time.Since(start)}, nil
}

// ResetFailOnceForTest clears the per-StdoutFile fail tracker.
// Test-only: in-process tests that drive multiple fail_once_then_ok
// scenarios across the same stdout path need a clean slate between
// table rows. Smoke tests that exec the binary do not need this since
// each invocation gets a fresh process and a fresh map.
func ResetFailOnceForTest() {
	failOnceState.Range(func(k, _ any) bool {
		failOnceState.Delete(k)
		return true
	})
}
