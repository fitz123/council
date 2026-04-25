package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
)

// pfExec is the preflight-test stub. Separate from main_test.go's
// stubExec so we can vary BinaryName independently of Name.
type pfExec struct {
	name string
	bin  string
}

func (e *pfExec) Name() string       { return e.name }
func (e *pfExec) BinaryName() string { return e.bin }
func (e *pfExec) Execute(_ context.Context, _ executor.Request) (executor.Response, error) {
	return executor.Response{}, nil
}

// TestPreflight_ResolvesRegisteredAndAvailable — happy path: executor
// is registered AND its BinaryName resolves on PATH. /bin/sh is
// present on every Unix CI host the project targets.
func TestPreflight_ResolvesRegisteredAndAvailable(t *testing.T) {
	executor.ResetForTest()
	t.Cleanup(func() { executor.ResetForTest() })
	executor.Register(&pfExec{name: "claude-code", bin: "sh"})

	profile := &config.Profile{
		Name: "default",
		Experts: []config.RoleConfig{
			{Name: "expert_1", Executor: "claude-code", Model: "x", Timeout: time.Second},
		},
	}
	if err := Preflight(profile); err != nil {
		t.Fatalf("Preflight(ok) = %v, want nil", err)
	}
}

// TestPreflight_BinaryMissing — executor is registered but its
// BinaryName resolves to a name that is not on PATH. Preflight must
// surface an error naming the binary AND the offending profile line.
func TestPreflight_BinaryMissing(t *testing.T) {
	executor.ResetForTest()
	t.Cleanup(func() { executor.ResetForTest() })
	const fakeBin = "council-preflight-missing-binary-xyz123"
	executor.Register(&pfExec{name: "claude-code", bin: fakeBin})

	profile := &config.Profile{
		Name: "default",
		Experts: []config.RoleConfig{
			{Name: "expert_99", Executor: "claude-code", Model: "x", Timeout: time.Second},
		},
	}
	err := Preflight(profile)
	if err == nil {
		t.Fatalf("Preflight(missing) = nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, fakeBin) {
		t.Errorf("error must name the binary %q; got %q", fakeBin, msg)
	}
	if !strings.Contains(msg, "expert_99") {
		t.Errorf("error must name the offending expert; got %q", msg)
	}
	if !strings.Contains(msg, "experts[0]") {
		t.Errorf("error must name the experts[N] index; got %q", msg)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error must wrap exec.ErrNotFound; got %v", err)
	}
}

// TestPreflight_ExecutorUnregistered — preflight tolerates being run
// before orchestrator.Validate (which catches this earlier) by also
// surfacing unknown-executor as a clear preflight error.
func TestPreflight_ExecutorUnregistered(t *testing.T) {
	executor.ResetForTest()
	t.Cleanup(func() { executor.ResetForTest() })

	profile := &config.Profile{
		Name: "default",
		Experts: []config.RoleConfig{
			{Name: "ghost", Executor: "no-such-executor", Model: "x", Timeout: time.Second},
		},
	}
	err := Preflight(profile)
	if err == nil {
		t.Fatalf("Preflight(unregistered) = nil, want error")
	}
	if !strings.Contains(err.Error(), "no-such-executor") {
		t.Errorf("error must name the unregistered executor; got %q", err)
	}
}

// initialRegistry snapshots the registry exactly once at package-var
// init time, BEFORE any test runs. Since side-effect imports in
// executors_release.go run their init() before package var
// initialization, this captures the post-import state — invariant
// against later ResetForTest() calls in other tests.
var initialRegistry = func() map[string]bool {
	snapshot := map[string]bool{}
	for _, n := range []string{"claude-code", "codex", "gemini-cli"} {
		if _, err := executor.Get(n); err == nil {
			snapshot[n] = true
		}
	}
	return snapshot
}()

// TestPreflight_AllRegisteredExecutorsResolveByDefault — proves the
// side-effect imports in executors_release.go actually register codex
// and gemini-cli alongside claude-code. Reads initialRegistry rather
// than calling executor.Get directly because other tests in the
// package call ResetForTest in their cleanup, which races test order.
// PATH coverage belongs to live tests + smoke F14; here we only assert
// registry presence at process startup.
func TestPreflight_AllRegisteredExecutorsResolveByDefault(t *testing.T) {
	for _, name := range []string{"claude-code", "codex", "gemini-cli"} {
		if !initialRegistry[name] {
			t.Errorf("executor %q not registered after side-effect import", name)
		}
	}
}
