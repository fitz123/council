package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
)

// runMainCmd is the shared driver for resume tests. Returns exit code +
// stdout/stderr. Caller has already chdir'd into the test workspace.
func runMainCmd(t *testing.T, ctx context.Context, argv ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(ctx, argv, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestResume_NoSessions — `council resume` with no sessions folder must exit
// 1 with the "no resumable session" message rather than crashing.
func TestResume_NoSessions(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, stderr := runMainCmd(t, context.Background(), "resume")
	if code != exitConfigError {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitConfigError, stderr)
	}
	if !strings.Contains(stderr, "no resumable session") {
		t.Errorf("stderr = %q, want hint about no resumable session", stderr)
	}
}

// TestResume_UnknownSession — explicit --session that does not exist must
// surface a clear error and exit 1.
func TestResume_UnknownSession(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	code, _, stderr := runMainCmd(t, context.Background(),
		"resume", "--session", "does-not-exist")
	if code != exitConfigError {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitConfigError, stderr)
	}
	if !strings.Contains(stderr, "does-not-exist") {
		t.Errorf("stderr missing session id: %q", stderr)
	}
}

// TestResume_InvalidSessionIDRejected — explicit --session values containing
// path separators or ".." must be rejected before we touch the filesystem.
// Otherwise filepath.Join(sessionsRoot, id) can escape .council/sessions/
// and point at an attacker-prepared directory whose profile.snapshot.yaml
// controls the executor/model/prompt that get dispatched to the child
// process.
func TestResume_InvalidSessionIDRejected(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	cases := []string{
		"../etc",
		"../../tmp/attack",
		"..",
		".",
		"foo/bar",
		`foo\bar`,
		"legit-name..suffix",
	}
	for _, id := range cases {
		code, _, stderr := runMainCmd(t, context.Background(),
			"resume", "--session", id)
		if code != exitConfigError {
			t.Errorf("id %q: exit = %d, want %d (stderr=%s)", id, code, exitConfigError, stderr)
		}
		if !strings.Contains(stderr, "invalid session id") {
			t.Errorf("id %q: stderr = %q, want 'invalid session id' hint", id, stderr)
		}
	}
}

// TestResume_ExplicitSessionRejectsFinal — `council resume --session <id>`
// must apply the D14 finality predicate just like the implicit newest-
// incomplete search. Otherwise, an operator could accidentally mutate a
// completed session's verdict/output by naming it directly (rewrite
// timestamps, possibly change the winner if ballots were re-run).
func TestResume_ExplicitSessionRejectsFinal(t *testing.T) {
	cwd := withCouncilDir(t, t.TempDir())
	t.Chdir(cwd)

	// Drive a clean run to completion so the session ends in a final state
	// (root .done written, verdict.json.status == "ok").
	registerStub(t, happyStub())
	if code, _, stderr := runMainCmd(t, context.Background(), "q"); code != exitOK {
		t.Fatalf("initial run exit = %d, want %d (stderr=%s)", code, exitOK, stderr)
	}
	sessions, _ := filepath.Glob(".council/sessions/*")
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(sessions))
	}
	sessID := filepath.Base(sessions[0])

	// Now try to resume the finalized session by explicit ID. Must be
	// rejected — without the finality check, orchestrator.Run would
	// happily rewrite verdict.json timestamps and output.md.
	code, _, stderr := runMainCmd(t, context.Background(),
		"resume", "--session", sessID)
	if code != exitConfigError {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitConfigError, stderr)
	}
	if !strings.Contains(stderr, "not resumable") {
		t.Errorf("stderr = %q, want hint about non-resumable session", stderr)
	}
}

// TestResume_AfterFullR2_RunsOnlyVote — happy v2 flow killed AFTER R2 but
// BEFORE the vote stage. On resume:
//   - all R1 + R2 stage .done markers present, R2 outputs cached
//   - resume runs ONLY the ballot subprocesses + finalizes verdict
//   - exit 0 with status=ok and the prior R2 bodies preserved
func TestResume_AfterFullR2_RunsOnlyVote(t *testing.T) {
	cwd := withCouncilDir(t, t.TempDir())
	t.Chdir(cwd)

	// First run: R1+R2 succeed, ballots fail with "killed" so no tally is
	// written. Verdict is recorded as status="error" (orchestrator's catch-all
	// for ballot failure), but the simulated kill below replaces it with
	// status="interrupted" so the resume predicate accepts it.
	roundOK := func(_ int, label, stdoutFile, _ string) (int, error) {
		return writeStdout("body-"+label)(stdoutFile, "")
	}
	var ballotCallsFirst int64
	stub1 := &stubExec{
		name:    "claude-code",
		onRound: roundOK,
		onBallot: func(_, _, _ string) (int, error) {
			atomic.AddInt64(&ballotCallsFirst, 1)
			return 1, errors.New("ballot killed mid-run")
		},
	}
	registerStub(t, stub1)
	if code, _, stderr := runMainCmd(t, context.Background(), "q"); code == exitOK {
		t.Fatalf("first run should not succeed (ballots failed); got exit %d stderr=%s", code, stderr)
	}
	if atomic.LoadInt64(&ballotCallsFirst) == 0 {
		t.Fatalf("first run did not invoke ballots; resume scenario invalid")
	}

	// Simulate "killed before final verdict" by overwriting verdict.json
	// with a non-final status (interrupted) and removing the root .done
	// marker so the resume predicate accepts the session.
	sessions, _ := filepath.Glob(".council/sessions/*")
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(sessions))
	}
	sess := sessions[0]
	_ = os.Remove(filepath.Join(sess, ".done"))
	if err := os.WriteFile(filepath.Join(sess, "verdict.json"),
		[]byte(`{"status": "interrupted"}`), 0o644); err != nil {
		t.Fatalf("rewrite verdict: %v", err)
	}
	// Wipe voting/tally.json + output.md from the prior run so we can
	// detect that the resume actually wrote them.
	_ = os.Remove(filepath.Join(sess, "voting", "tally.json"))
	_ = os.Remove(filepath.Join(sess, "output.md"))

	// Second run: ballots succeed, expert subprocesses MUST NOT run again.
	var roundCallsSecond, ballotCallsSecond int64
	stub2 := &stubExec{
		name: "claude-code",
		onRound: func(_ int, _, _, _ string) (int, error) {
			atomic.AddInt64(&roundCallsSecond, 1)
			return 1, errors.New("expert should not be re-spawned on resume")
		},
		onBallot: func(_, stdoutFile, _ string) (int, error) {
			atomic.AddInt64(&ballotCallsSecond, 1)
			return writeStdout("VOTE: A\n")(stdoutFile, "")
		},
	}
	registerStub(t, stub2)
	code, stdout, stderr := runMainCmd(t, context.Background(), "resume")
	if code != exitOK {
		t.Fatalf("resume exit = %d, want %d (stderr=%s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "body-A") {
		t.Errorf("stdout = %q, want winner body-A", stdout)
	}
	if got := atomic.LoadInt64(&roundCallsSecond); got != 0 {
		t.Errorf("expert re-spawns on resume = %d, want 0 (R1+R2 .done should short-circuit)", got)
	}
	if got := atomic.LoadInt64(&ballotCallsSecond); got != 3 {
		t.Errorf("ballot calls on resume = %d, want 3 (no per-voter .done)", got)
	}
	// Verdict status now ok, root .done present.
	v, err := os.ReadFile(filepath.Join(sess, "verdict.json"))
	if err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if !strings.Contains(string(v), `"status": "ok"`) {
		t.Errorf("verdict missing status=ok: %s", v)
	}
	if _, err := os.Stat(filepath.Join(sess, ".done")); err != nil {
		t.Errorf("root .done missing after resume: %v", err)
	}
}

// TestResume_PartialR2 — crash mid-R2 with one expert .done, others not.
// On resume the missing experts re-run; the .done one is reused.
func TestResume_PartialR2(t *testing.T) {
	cwd := withCouncilDir(t, t.TempDir())
	t.Chdir(cwd)

	// First run: R1 succeeds for all, R2 succeeds for whichever expert is
	// labeled "A" but fails for B and C. Carry-forward marks B and C as
	// "carried" (writes .done). To exercise "missing R2 .done", we
	// post-process the session by removing B's and C's .done markers.
	stub1 := &stubExec{
		name: "claude-code",
		onRound: func(round int, label, stdoutFile, _ string) (int, error) {
			if round == 2 && label != "A" {
				return 1, errors.New("R2 not yet for " + label)
			}
			return writeStdout("r"+strconv.Itoa(round)+"-"+label)(stdoutFile, "")
		},
		onBallot: func(_, _, _ string) (int, error) {
			return 1, errors.New("ballot killed first run")
		},
	}
	registerStub(t, stub1)
	_, _, _ = runMainCmd(t, context.Background(), "q")

	sessions, _ := filepath.Glob(".council/sessions/*")
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(sessions))
	}
	sess := sessions[0]
	// Remove B's and C's R2 .done so resume sees them as "missing".
	for _, label := range []string{"B", "C"} {
		dir := filepath.Join(sess, "rounds", "2", "experts", label)
		_ = os.Remove(filepath.Join(dir, ".done"))
		_ = os.Remove(filepath.Join(dir, "output.md"))
	}
	// Force the session into resumable state.
	_ = os.Remove(filepath.Join(sess, ".done"))
	_ = os.WriteFile(filepath.Join(sess, "verdict.json"),
		[]byte(`{"status": "interrupted"}`), 0o644)
	// Wipe ballot artifacts so resume's ballot stage sees a clean slate.
	_ = os.RemoveAll(filepath.Join(sess, "voting"))

	// Second run: R1 must NOT re-run (all .done). R2 for A must NOT re-run.
	// R2 for B and C must re-run (succeed this time). Ballots run.
	var r1Calls, r2Calls int64
	stub2 := &stubExec{
		name: "claude-code",
		onRound: func(round int, label, stdoutFile, _ string) (int, error) {
			if round == 1 {
				atomic.AddInt64(&r1Calls, 1)
				return writeStdout("r1-"+label)(stdoutFile, "")
			}
			atomic.AddInt64(&r2Calls, 1)
			if label == "A" {
				return 1, errors.New("A R2 should not be re-spawned (had .done)")
			}
			return writeStdout("r2-"+label)(stdoutFile, "")
		},
		onBallot: func(_, stdoutFile, _ string) (int, error) {
			return writeStdout("VOTE: A\n")(stdoutFile, "")
		},
	}
	registerStub(t, stub2)
	code, _, stderr := runMainCmd(t, context.Background(), "resume")
	if code != exitOK {
		t.Fatalf("resume exit = %d, want %d (stderr=%s)", code, exitOK, stderr)
	}
	if got := atomic.LoadInt64(&r1Calls); got != 0 {
		t.Errorf("R1 re-spawns on resume = %d, want 0", got)
	}
	// R2: 2 spawns expected (B and C). A short-circuits.
	if got := atomic.LoadInt64(&r2Calls); got != 2 {
		t.Errorf("R2 spawns on resume = %d, want 2 (B+C only, A reused)", got)
	}
	// B and C now have .done markers and output.
	for _, label := range []string{"B", "C"} {
		dir := filepath.Join(sess, "rounds", "2", "experts", label)
		if _, err := os.Stat(filepath.Join(dir, ".done")); err != nil {
			t.Errorf("%s .done missing after resume: %v", label, err)
		}
	}
}

// TestResume_SIGINTPartialVerdictNotFinal — a SIGINT mid-run leaves a
// verdict.json with status="interrupted" (a non-final status). The resume
// predicate must pick this session up despite verdict.json's existence,
// confirming finality-not-existence is the predicate.
func TestResume_SIGINTPartialVerdictNotFinal(t *testing.T) {
	cwd := withCouncilDir(t, t.TempDir())
	t.Chdir(cwd)

	// Drive a real interrupt: each expert blocks until ctx is cancelled.
	started := make(chan struct{}, 16)
	executor.ResetForTest()
	executor.Register(&interruptibleStub{name: "claude-code", started: started})
	t.Cleanup(func() { executor.ResetForTest() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	code, _, stderr := runMainCmd(t, ctx, "q")
	if code != exitInterrupted {
		t.Fatalf("first run exit = %d, want %d (stderr=%s)", code, exitInterrupted, stderr)
	}
	sessions, _ := filepath.Glob(".council/sessions/*")
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(sessions))
	}
	sess := sessions[0]
	v, err := os.ReadFile(filepath.Join(sess, "verdict.json"))
	if err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if !strings.Contains(string(v), `"status": "interrupted"`) {
		t.Fatalf("first run verdict not interrupted: %s", v)
	}
	if _, err := os.Stat(filepath.Join(sess, ".done")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root .done unexpectedly present after interrupt: %v", err)
	}

	// Without a single .done under rounds/, the first run cancelled before
	// any expert finished — so this isn't a resumable scenario by D14's
	// rules. Pre-seed one R1 .done to simulate "expert finished mid-run,
	// then SIGINT arrived" — the case the predicate is designed to handle.
	aDir := filepath.Join(sess, "rounds", "1", "experts", "A")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "output.md"), []byte("from-A\n"), 0o644); err != nil {
		t.Fatalf("seed A output: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".done"), nil, 0o644); err != nil {
		t.Fatalf("seed A .done: %v", err)
	}

	// Second run: a normal happyStub completes everything. Resume picks the
	// session up despite verdict.json existing — its status="interrupted" is
	// non-final.
	registerStub(t, happyStub())
	code, stdout, stderr := runMainCmd(t, context.Background(), "resume")
	if code != exitOK {
		t.Fatalf("resume exit = %d, want %d (stderr=%s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "body-A") {
		t.Errorf("stdout = %q, want winner body-A", stdout)
	}
	v2, err := os.ReadFile(filepath.Join(sess, "verdict.json"))
	if err != nil {
		t.Fatalf("read verdict after resume: %v", err)
	}
	var verdict struct{ Status string }
	if err := json.Unmarshal(v2, &verdict); err != nil {
		t.Fatalf("parse resumed verdict: %v", err)
	}
	if verdict.Status != "ok" {
		t.Errorf("resumed verdict status = %q, want ok", verdict.Status)
	}
}
