//go:build testbinary

package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
)

// makeReq returns a Request whose Stdout/Stderr files live in t.TempDir,
// so each subtest gets isolated paths. Timeout is generous; the slow
// behavior overrides it via ctx anyway.
func makeReq(t *testing.T, name string) executor.Request {
	t.Helper()
	dir := t.TempDir()
	return executor.Request{
		Prompt:     "expert prompt body",
		Model:      "sonnet",
		Timeout:    5 * time.Second,
		StdoutFile: filepath.Join(dir, name+"-stdout.md"),
		StderrFile: filepath.Join(dir, name+"-stderr.log"),
	}
}

// withEnv sets an env var for the duration of the test and restores
// the prior value on cleanup. Avoids leaking state between subtests
// that share the same global env.
func withEnv(t *testing.T, key, val string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Setenv(key, val); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestMock_TrivialDefault(t *testing.T) {
	withEnv(t, EnvName, "")
	req := makeReq(t, "trivial-default")
	m := &Mock{}
	resp, err := m.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("trivial: unexpected error: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("trivial: exit code = %d, want 0", resp.ExitCode)
	}
	body, rerr := os.ReadFile(req.StdoutFile)
	if rerr != nil {
		t.Fatalf("read stdout: %v", rerr)
	}
	if string(body) != "trivial mock answer\n" {
		t.Fatalf("trivial: stdout = %q, want %q", body, "trivial mock answer\n")
	}
}

func TestMock_TrivialExplicit(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	req := makeReq(t, "trivial-explicit")
	m := &Mock{}
	if _, err := m.Execute(context.Background(), req); err != nil {
		t.Fatalf("trivial: %v", err)
	}
	if _, err := os.Stat(req.StdoutFile); err != nil {
		t.Fatalf("expected stdout file: %v", err)
	}
}

func TestMock_SlowBlocksUntilCancel(t *testing.T) {
	withEnv(t, EnvName, "slow")
	req := makeReq(t, "slow")
	m := &Mock{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var resp executor.Response
	var err error
	go func() {
		resp, err = m.Execute(ctx, req)
		close(done)
	}()

	// Verify the call really blocks — give it a beat, expect no return.
	select {
	case <-done:
		t.Fatalf("slow: returned before cancellation")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("slow: did not return within 1s after cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("slow: err = %v, want context.Canceled", err)
	}
	if resp.Duration <= 0 {
		t.Fatalf("slow: duration not set (got %s)", resp.Duration)
	}
}

func TestMock_FailOnceThenOk(t *testing.T) {
	withEnv(t, EnvName, "fail_once_then_ok")
	t.Cleanup(ResetFailOnceForTest)
	ResetFailOnceForTest()
	req := makeReq(t, "fail-once")
	m := &Mock{}

	// First call: fails, stderr file written.
	resp1, err1 := m.Execute(context.Background(), req)
	if err1 == nil {
		t.Fatalf("first call: expected error, got nil")
	}
	if resp1.ExitCode != 1 {
		t.Fatalf("first call: exit code = %d, want 1", resp1.ExitCode)
	}
	stderrBytes, rerr := os.ReadFile(req.StderrFile)
	if rerr != nil {
		t.Fatalf("first call: read stderr: %v", rerr)
	}
	if !strings.Contains(string(stderrBytes), "first attempt fails") {
		t.Fatalf("first call: stderr = %q, missing marker", stderrBytes)
	}

	// Second call: succeeds with the same StdoutFile.
	resp2, err2 := m.Execute(context.Background(), req)
	if err2 != nil {
		t.Fatalf("second call: %v", err2)
	}
	if resp2.ExitCode != 0 {
		t.Fatalf("second call: exit code = %d, want 0", resp2.ExitCode)
	}
	body, rerr := os.ReadFile(req.StdoutFile)
	if rerr != nil {
		t.Fatalf("second call: read stdout: %v", rerr)
	}
	if !strings.Contains(string(body), "succeeded after retry") {
		t.Fatalf("second call: stdout = %q, missing success marker", body)
	}
}

func TestMock_FailOnceThenOk_DistinctPaths(t *testing.T) {
	// Different StdoutFile paths each get their own first-fail.
	withEnv(t, EnvName, "fail_once_then_ok")
	t.Cleanup(ResetFailOnceForTest)
	ResetFailOnceForTest()
	m := &Mock{}

	req1 := makeReq(t, "path-a")
	req2 := makeReq(t, "path-b")
	if _, err := m.Execute(context.Background(), req1); err == nil {
		t.Fatalf("req1: expected first-fail error")
	}
	if _, err := m.Execute(context.Background(), req2); err == nil {
		t.Fatalf("req2: expected first-fail error")
	}
}

func TestMock_EchoStdinLength(t *testing.T) {
	withEnv(t, EnvName, "echo-stdin-length")
	req := makeReq(t, "echo")
	req.Prompt = strings.Repeat("y\n", 100) // 200 bytes
	m := &Mock{}
	resp, err := m.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("echo: exit code = %d, want 0", resp.ExitCode)
	}
	body, rerr := os.ReadFile(req.StdoutFile)
	if rerr != nil {
		t.Fatalf("read stdout: %v", rerr)
	}
	want := "[stdin-bytes=200]\n"
	if string(body) != want {
		t.Fatalf("echo: stdout = %q, want %q", body, want)
	}
}

func TestMock_UnknownBehavior(t *testing.T) {
	withEnv(t, EnvName, "no-such-mode")
	req := makeReq(t, "unknown")
	m := &Mock{}
	_, err := m.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for unknown behavior")
	}
	if !strings.Contains(err.Error(), "no-such-mode") {
		t.Fatalf("error %q should name the bad behavior", err)
	}
}

// TestMock_BallotDefaultVotesA verifies the ballot override: any request
// whose StdoutFile lives under `voting/votes/` produces `VOTE: A\n`, no
// matter which expert behavior is selected. This is the glue that lets
// F3/F4/F6 reach exit 0 under v2 — their expert-stage behavior doesn't
// know about ballots, but the dispatcher does.
func TestMock_BallotDefaultVotesA(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	dir := t.TempDir()
	votesDir := filepath.Join(dir, "voting", "votes")
	if err := os.MkdirAll(votesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	req := executor.Request{
		Prompt:     "ballot prompt",
		Model:      "sonnet",
		Timeout:    time.Second,
		StdoutFile: filepath.Join(votesDir, "B.txt"),
		StderrFile: filepath.Join(votesDir, "B.stderr.log"),
	}
	m := &Mock{}
	if _, err := m.Execute(context.Background(), req); err != nil {
		t.Fatalf("ballot: %v", err)
	}
	body, err := os.ReadFile(req.StdoutFile)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if string(body) != "VOTE: A\n" {
		t.Fatalf("ballot body = %q, want %q", body, "VOTE: A\n")
	}
}

// TestMock_BallotSelfVoteTie verifies self_vote_tie: every voter votes for
// its own label, producing an N-way tie in the tally. Exercised end-to-end
// by the F12 smoke test.
func TestMock_BallotSelfVoteTie(t *testing.T) {
	withEnv(t, EnvName, "self_vote_tie")
	dir := t.TempDir()
	votesDir := filepath.Join(dir, "voting", "votes")
	if err := os.MkdirAll(votesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, label := range []string{"A", "B", "C"} {
		req := executor.Request{
			Prompt:     "ballot prompt",
			Model:      "sonnet",
			Timeout:    time.Second,
			StdoutFile: filepath.Join(votesDir, label+".txt"),
			StderrFile: filepath.Join(votesDir, label+".stderr.log"),
		}
		m := &Mock{}
		if _, err := m.Execute(context.Background(), req); err != nil {
			t.Fatalf("ballot %s: %v", label, err)
		}
		body, err := os.ReadFile(req.StdoutFile)
		if err != nil {
			t.Fatalf("read %s: %v", label, err)
		}
		want := "VOTE: " + label + "\n"
		if string(body) != want {
			t.Fatalf("ballot %s body = %q, want %q", label, body, want)
		}
	}
}

// TestMock_ForgeFenceR1 verifies the forgery mode: the alphabetically-first
// R1 expert emits a line-anchored `=== … ===` fence that prompt.CheckForgery
// must reject. Other expert paths and ballots behave normally.
func TestMock_ForgeFenceR1(t *testing.T) {
	withEnv(t, EnvName, "forge_fence_r1")
	dir := t.TempDir()

	// Label A R1 path — should produce a forged fence.
	r1a := filepath.Join(dir, "rounds", "1", "experts", "A")
	if err := os.MkdirAll(r1a, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reqA := executor.Request{
		StdoutFile: filepath.Join(r1a, "output.md"),
		StderrFile: filepath.Join(r1a, "stderr.log"),
		Timeout:    time.Second,
	}
	m := &Mock{}
	if _, err := m.Execute(context.Background(), reqA); err != nil {
		t.Fatalf("A R1: %v", err)
	}
	body, err := os.ReadFile(reqA.StdoutFile)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if !strings.Contains(string(body), "=== EXPERT:") {
		t.Fatalf("A R1 body %q should contain a forged fence", body)
	}

	// Label B R1 path — should produce a clean trivial output.
	r1b := filepath.Join(dir, "rounds", "1", "experts", "B")
	if err := os.MkdirAll(r1b, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reqB := executor.Request{
		StdoutFile: filepath.Join(r1b, "output.md"),
		StderrFile: filepath.Join(r1b, "stderr.log"),
		Timeout:    time.Second,
	}
	if _, err := m.Execute(context.Background(), reqB); err != nil {
		t.Fatalf("B R1: %v", err)
	}
	bodyB, err := os.ReadFile(reqB.StdoutFile)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if strings.Contains(string(bodyB), "=== EXPERT:") {
		t.Fatalf("B R1 body %q should not contain a fence", bodyB)
	}
}

func TestMock_RegistersUnderClaudeCode(t *testing.T) {
	// init() in this package registers the Mock under "claude-code".
	// The release-binary ClaudeCode would do the same, but its
	// subpackage is not imported in this test binary — so executor.Get
	// resolves to *our* Mock, which is the substitution that lets
	// smoke tests run the default profile against stubs.
	got, err := executor.Get("claude-code")
	if err != nil {
		t.Fatalf("Get claude-code: %v", err)
	}
	if _, ok := got.(*Mock); !ok {
		t.Fatalf("registered executor type = %T, want *Mock", got)
	}
}
