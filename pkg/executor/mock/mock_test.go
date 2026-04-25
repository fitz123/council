//go:build testbinary

package mock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/prompt"
)

// decodeJSON unmarshals a single JSON line into v. Local helper for the
// call-log tests so each test row reads with the same shape.
func decodeJSON(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

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

func TestMock_NameAndBinaryName(t *testing.T) {
	// Mock must mirror ClaudeCode's identity so `-tags testbinary` smoke
	// builds keep routing the default profile's `executor: claude-code`
	// to the mock, AND keep preflight's exec.LookPath resolving to a real
	// `claude` binary on PATH. BinaryName != "mock" is load-bearing — see
	// the preflight contract in cmd/council/preflight.go.
	m := &Mock{}
	if got := m.Name(); got != "claude-code" {
		t.Errorf("Name() = %q, want claude-code", got)
	}
	if got := m.BinaryName(); got != "claude" {
		t.Errorf("BinaryName() = %q, want claude", got)
	}
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
	// Close the loop: the point of the forgery mode is to exercise the
	// reject path. If doForgeFenceR1 ever emits a shape the tightened
	// regex no longer matches (as happened once pre-ADR-0011 with the
	// 13-char "forged0000000" sentinel), F9 silently turns into a no-op.
	// Verify prompt.CheckForgery actually rejects this body against a
	// session nonce different from the forged literal.
	if err := prompt.CheckForgery(string(body), "00000000000000ff"); !errors.Is(err, prompt.ErrForgedFence) {
		t.Fatalf("A R1 body must trip ErrForgedFence; got %v\nbody: %q", err, body)
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

// TestMock_RecordsAllowedToolsAndPermissionMode verifies the package-level
// Execute call log captures the per-call AllowedTools / PermissionMode
// pair. F13 (experts always with WebSearch+WebFetch) and F17 (ballots
// always tools-off) drive their assertions through RecordedCalls.
//
// Table-driven across the live (executor.Request, want CallRecord) shapes
// the debate engine actually emits: nil/empty for ballots and v1, a full
// list for v2 expert spawns, and a partial list to confirm we don't
// short-circuit when only one of the fields is set.
func TestMock_RecordsAllowedToolsAndPermissionMode(t *testing.T) {
	withEnv(t, EnvName, "trivial")

	cases := []struct {
		name     string
		req      executor.Request
		wantTool []string
		wantMode string
	}{
		{
			name:     "v1_defaults_nil_and_empty",
			req:      executor.Request{},
			wantTool: nil,
			wantMode: "",
		},
		{
			name: "expert_spawn_full_tools_bypass_mode",
			req: executor.Request{
				AllowedTools:   []string{"WebSearch", "WebFetch"},
				PermissionMode: "bypassPermissions",
			},
			wantTool: []string{"WebSearch", "WebFetch"},
			wantMode: "bypassPermissions",
		},
		{
			name: "single_tool_no_mode",
			req: executor.Request{
				AllowedTools: []string{"WebFetch"},
			},
			wantTool: []string{"WebFetch"},
			wantMode: "",
		},
		{
			name: "mode_only_no_tools",
			req: executor.Request{
				PermissionMode: "bypassPermissions",
			},
			wantTool: nil,
			wantMode: "bypassPermissions",
		},
		{
			name: "empty_slice_distinct_from_nil_records_as_nil",
			req: executor.Request{
				AllowedTools: []string{},
			},
			wantTool: nil,
			wantMode: "",
		},
	}

	m := &Mock{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetCallsForTest()
			t.Cleanup(ResetCallsForTest)
			req := tc.req
			req.StdoutFile = filepath.Join(t.TempDir(), tc.name+"-stdout.md")
			req.StderrFile = filepath.Join(t.TempDir(), tc.name+"-stderr.log")
			if _, err := m.Execute(context.Background(), req); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			got := RecordedCalls()
			if len(got) != 1 {
				t.Fatalf("RecordedCalls len = %d, want 1", len(got))
			}
			rec := got[0]
			if rec.StdoutFile != req.StdoutFile {
				t.Errorf("StdoutFile = %q, want %q", rec.StdoutFile, req.StdoutFile)
			}
			if rec.PermissionMode != tc.wantMode {
				t.Errorf("PermissionMode = %q, want %q", rec.PermissionMode, tc.wantMode)
			}
			if !slicesEqual(rec.AllowedTools, tc.wantTool) {
				t.Errorf("AllowedTools = %v, want %v", rec.AllowedTools, tc.wantTool)
			}
		})
	}
}

// TestMock_RecordedCalls_SnapshotIsCopy verifies the returned slice is
// independent of the internal log: mutating the snapshot must not affect
// later RecordedCalls() reads, and inner slices must be copies too so
// tests can sort/edit without corrupting state for sibling assertions.
func TestMock_RecordedCalls_SnapshotIsCopy(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	ResetCallsForTest()
	t.Cleanup(ResetCallsForTest)

	req := makeReq(t, "snapshot")
	req.AllowedTools = []string{"WebSearch", "WebFetch"}
	m := &Mock{}
	if _, err := m.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	snap := RecordedCalls()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	// Mutate the snapshot's inner slice — must not leak back to the log.
	snap[0].AllowedTools[0] = "MUTATED"

	again := RecordedCalls()
	if again[0].AllowedTools[0] != "WebSearch" {
		t.Fatalf("snapshot mutation leaked into recorder: got %q, want WebSearch", again[0].AllowedTools[0])
	}
}

// TestMock_RecordedCalls_AppendOrder verifies calls are recorded in the
// order they happen — F13/F17 assertions iterate the log to match
// stage+label against expected tool sets per call.
func TestMock_RecordedCalls_AppendOrder(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	ResetCallsForTest()
	t.Cleanup(ResetCallsForTest)

	dir := t.TempDir()
	m := &Mock{}
	for _, name := range []string{"first", "second", "third"} {
		req := executor.Request{
			StdoutFile: filepath.Join(dir, name+".md"),
			StderrFile: filepath.Join(dir, name+".log"),
			Timeout:    time.Second,
		}
		if _, err := m.Execute(context.Background(), req); err != nil {
			t.Fatalf("Execute %s: %v", name, err)
		}
	}
	got := RecordedCalls()
	if len(got) != 3 {
		t.Fatalf("got %d calls, want 3", len(got))
	}
	for i, name := range []string{"first", "second", "third"} {
		if !strings.HasSuffix(got[i].StdoutFile, name+".md") {
			t.Errorf("calls[%d].StdoutFile = %q, want suffix %q", i, got[i].StdoutFile, name+".md")
		}
	}
}

// TestMock_CallLogFile_AppendsJSONLines verifies the COUNCIL_MOCK_CALL_LOG
// sidecar: when set, each Execute appends one JSON object per line with
// the same StdoutFile / AllowedTools / PermissionMode fields the
// in-process recorder captures. Smoke tests that exec the testbinary
// rely on this file because they cannot reach RecordedCalls() across
// the process boundary.
func TestMock_CallLogFile_AppendsJSONLines(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	withEnv(t, EnvCallLog, logPath)
	t.Cleanup(ResetCallsForTest)
	ResetCallsForTest()

	dir := t.TempDir()
	m := &Mock{}
	cases := []struct {
		name string
		req  executor.Request
	}{
		{
			name: "expert_full_tools",
			req: executor.Request{
				AllowedTools:   []string{"WebSearch", "WebFetch"},
				PermissionMode: "bypassPermissions",
			},
		},
		{
			name: "ballot_no_tools",
			req:  executor.Request{}, // ballot path drops AllowedTools/PermissionMode
		},
	}
	for _, tc := range cases {
		req := tc.req
		req.StdoutFile = filepath.Join(dir, tc.name+".md")
		req.StderrFile = filepath.Join(dir, tc.name+".log")
		req.Timeout = time.Second
		if _, err := m.Execute(context.Background(), req); err != nil {
			t.Fatalf("Execute %s: %v", tc.name, err)
		}
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("call log lines = %d, want %d\n%s", len(lines), len(cases), body)
	}

	for i, tc := range cases {
		var got CallRecord
		if err := decodeJSON(lines[i], &got); err != nil {
			t.Fatalf("line %d decode: %v\n%s", i, err, lines[i])
		}
		wantSuffix := tc.name + ".md"
		if !strings.HasSuffix(got.StdoutFile, wantSuffix) {
			t.Errorf("line %d StdoutFile = %q, want suffix %q", i, got.StdoutFile, wantSuffix)
		}
		if !slicesEqual(got.AllowedTools, tc.req.AllowedTools) {
			t.Errorf("line %d AllowedTools = %v, want %v", i, got.AllowedTools, tc.req.AllowedTools)
		}
		if got.PermissionMode != tc.req.PermissionMode {
			t.Errorf("line %d PermissionMode = %q, want %q", i, got.PermissionMode, tc.req.PermissionMode)
		}
	}
}

// TestMock_CallLogFile_DisabledByDefault verifies that without the env
// var set, recordCall does not create a log file — preserves v1
// silent-mock behavior for tests that don't opt in.
func TestMock_CallLogFile_DisabledByDefault(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	withEnv(t, EnvCallLog, "")
	t.Cleanup(ResetCallsForTest)
	ResetCallsForTest()

	logPath := filepath.Join(t.TempDir(), "should-not-exist.jsonl")
	req := makeReq(t, "no-log")
	m := &Mock{}
	if _, err := m.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("call log %q should not exist (err=%v)", logPath, err)
	}
}

// TestMock_RecordedCalls_ResetClearsLog ensures ResetCallsForTest
// actually empties the package-level slice between table rows.
func TestMock_RecordedCalls_ResetClearsLog(t *testing.T) {
	withEnv(t, EnvName, "trivial")
	ResetCallsForTest()
	t.Cleanup(ResetCallsForTest)

	req := makeReq(t, "reset")
	m := &Mock{}
	if _, err := m.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(RecordedCalls()) != 1 {
		t.Fatalf("pre-reset len = %d, want 1", len(RecordedCalls()))
	}
	ResetCallsForTest()
	if got := RecordedCalls(); len(got) != 0 {
		t.Fatalf("post-reset len = %d, want 0", len(got))
	}
}

// slicesEqual is a local nil-tolerant equality check so the tests can
// distinguish nil from empty (we deliberately normalize empty→nil in
// recordCall to match v1 behaviour).
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if (a == nil) != (b == nil) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
