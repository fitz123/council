package debate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/fitz123/council/pkg/executor"
)

// TestRunRound1_ResumeReusesDone verifies the resume-path shortcut: when a
// prior run left rounds/1/experts/<label>/{output.md,.done} behind, a second
// RunRound1 call does NOT spawn the subprocess for that label and instead
// returns the stored body. R1 idempotency is the foundation for D14 resume
// behaviour under "SIGINT mid-run, replay later" scenarios.
func TestRunRound1_ResumeReusesDone(t *testing.T) {
	var spawns int64
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			atomic.AddInt64(&spawns, 1)
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "fresh from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "1357911135791113", exec)

	// Pre-seed rounds/1/experts/A/ as if a prior run had completed.
	aDir := s.RoundExpertDir(1, "A")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "output.md"), []byte("cached A body\n"), 0o644); err != nil {
		t.Fatalf("write A output.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".done"), nil, 0o644); err != nil {
		t.Fatalf("touch A .done: %v", err)
	}

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}

	a := byLabel(outs, "A")
	if a == nil {
		t.Fatalf("A missing from outs")
	}
	if a.Participation != "ok" {
		t.Errorf("A participation = %q, want ok (resumed)", a.Participation)
	}
	if a.Body != "cached A body\n" {
		t.Errorf("A body = %q, want cached body (resume should not re-spawn)", a.Body)
	}
	// B and C were NOT pre-seeded, so they ran normally.
	for _, label := range []string{"B", "C"} {
		o := byLabel(outs, label)
		if o.Participation != "ok" {
			t.Errorf("%s participation = %q, want ok", label, o.Participation)
		}
	}
	// A must NOT have been spawned; B and C must have been.
	if got := atomic.LoadInt64(&spawns); got != 2 {
		t.Errorf("subprocess spawns = %d, want 2 (A reused, B+C fresh)", got)
	}
}

// TestRunRound1_ResumePersistsFailure verifies the .failed tombstone freezes
// R1 drops across resume (D3: "expert DROPPED from session" is permanent).
// Without persistence, a previously-failed expert would get re-spawned on
// resume, potentially succeed, and end up in a cohort whose existing R2 .done
// outputs were built without it — producing an inconsistent debate.
func TestRunRound1_ResumePersistsFailure(t *testing.T) {
	var spawns int64
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			atomic.AddInt64(&spawns, 1)
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "C" {
				return "", errors.New("C keeps failing")
			}
			return "ok " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "feeffeeffeeffeef", exec)

	// First R1 pass: C fails, A and B succeed. Quorum=1 so no error.
	if _, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q"); err != nil {
		t.Fatalf("first RunRound1: %v", err)
	}
	cFailedPath := filepath.Join(s.RoundExpertDir(1, "C"), ".failed")
	if _, err := os.Stat(cFailedPath); err != nil {
		t.Fatalf("C .failed marker not written after R1 failure: %v", err)
	}

	// Second R1 pass (simulating a resume after mid-R2 SIGINT): the exec is
	// replaced with a guard that fails on any spawn — no expert should
	// re-spawn on resume. A and B short-circuit via .done; C is gated by
	// its .failed tombstone (D3 permanence).
	atomic.StoreInt64(&spawns, 0)
	exec.fn = func(ctx context.Context, req executor.Request, _ int) (string, error) {
		atomic.AddInt64(&spawns, 1)
		return "", errors.New("no R1 expert should re-spawn on resume")
	}
	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("second RunRound1: %v", err)
	}
	c := byLabel(outs, "C")
	if c == nil {
		t.Fatalf("C missing from outs")
	}
	if c.Participation != "failed" {
		t.Errorf("C participation = %q, want failed (tombstone must freeze drop)", c.Participation)
	}
	if got := atomic.LoadInt64(&spawns); got != 0 {
		t.Errorf("subprocess spawns on resume = %d, want 0 (A/B short-circuit via .done, C via .failed)", got)
	}
}

// TestRunRound2_ResumeReusesDone mirrors TestRunRound1_ResumeReusesDone for
// R2. Pre-seeding an R2 .done + output.md causes RunRound2 to short-circuit
// that expert rather than re-invoking the peer-aware subprocess — the resume
// predicate's job.
func TestRunRound2_ResumeReusesDone(t *testing.T) {
	var spawns int64
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			atomic.AddInt64(&spawns, 1)
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "fresh r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "2468101214161820", exec)
	r1 := r1For(labeled)

	// Pre-seed rounds/2/experts/A/ as if a prior mid-R2 run had completed A.
	aDir := s.RoundExpertDir(2, "A")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir A R2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "output.md"), []byte("cached r2 A\n"), 0o644); err != nil {
		t.Fatalf("write A output.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".done"), nil, 0o644); err != nil {
		t.Fatalf("touch A .done: %v", err)
	}

	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	a := byLabel(outs, "A")
	if a.Participation != "ok" {
		t.Errorf("A participation = %q, want ok (resumed)", a.Participation)
	}
	if a.Body != "cached r2 A\n" {
		t.Errorf("A body = %q, want cached r2 body", a.Body)
	}
	// 2 spawns expected — B and C, not A.
	if got := atomic.LoadInt64(&spawns); got != 2 {
		t.Errorf("R2 subprocess spawns = %d, want 2 (A reused)", got)
	}
}

// TestRunRound2_ResumeRestoresCarriedStatus verifies the carry-forward
// marker (.carried) preserves the "carried" participation label across a
// resume boundary. Without the marker, resume reads the R1 body back from
// R2/output.md and reports "ok", silently erasing the fact that the R2
// subprocess actually failed.
func TestRunRound2_ResumeRestoresCarriedStatus(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "fresh r2\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "7777888899990000", exec)
	r1 := r1For(labeled)

	// Pre-seed rounds/2/experts/A/ as a carried stage: R1 body in output.md,
	// .done present, .carried present. Resume must report A as "carried".
	aDir := s.RoundExpertDir(2, "A")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir A R2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "output.md"), []byte("r1 from A\n"), 0o644); err != nil {
		t.Fatalf("write A output.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".carried"), nil, 0o644); err != nil {
		t.Fatalf("write A .carried: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".done"), nil, 0o644); err != nil {
		t.Fatalf("touch A .done: %v", err)
	}

	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	a := byLabel(outs, "A")
	if a == nil {
		t.Fatalf("A missing from outs")
	}
	if a.Participation != "carried" {
		t.Errorf("A participation = %q, want carried (marker must survive resume)", a.Participation)
	}
	if a.Body != "r1 from A\n" {
		t.Errorf("A body = %q, want preserved R1 bytes", a.Body)
	}
}

// TestRunRound2_SuccessScrubsStaleCarriedMarker — a stale .carried marker
// from a prior aborted carry-forward attempt (write .carried OK, write
// .done failed) must NOT survive a later successful R2 run. Without the
// scrub, a future resume would read .done + .carried and mislabel the
// expert as Participation="carried" even though the body on disk is a
// real R2 output.
func TestRunRound2_SuccessScrubsStaleCarriedMarker(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "fresh r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "abcdef0123456789", exec)
	r1 := r1For(labeled)

	// Pre-seed rounds/2/experts/A/.carried WITHOUT .done — simulates the
	// narrow crash window where a prior carry-forward wrote .carried but
	// died before TouchDone. R2 must re-run (no .done), succeed, and
	// scrub .carried.
	aDir := s.RoundExpertDir(2, "A")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir A R2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".carried"), nil, 0o644); err != nil {
		t.Fatalf("seed A .carried: %v", err)
	}

	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	a := byLabel(outs, "A")
	if a == nil {
		t.Fatalf("A missing from outs")
	}
	if a.Participation != "ok" {
		t.Errorf("A participation = %q, want ok (fresh R2 success)", a.Participation)
	}
	if _, err := os.Stat(filepath.Join(aDir, ".carried")); !os.IsNotExist(err) {
		t.Errorf(".carried still present after R2 success: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(aDir, ".done")); err != nil {
		t.Errorf(".done missing after R2 success: %v", err)
	}
}

// TestRunBallot_ResumeReusesVoteFile verifies RunBallot treats voting/votes/
// <label>.txt as the resume marker: an existing, parseable ballot file with
// a vote for an active label is reused without re-spawning the voter.
func TestRunBallot_ResumeReusesVoteFile(t *testing.T) {
	var spawns int64
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			atomic.AddInt64(&spawns, 1)
			return "VOTE: A\n", nil
		},
	}
	s, _, labeled := setupRoundTest(t, "1a2b3c4d5e6f7788", exec)

	// Pre-seed A's ballot file with a valid vote.
	votesDir := filepath.Join(s.Path, "voting", "votes")
	if err := os.MkdirAll(votesDir, 0o755); err != nil {
		t.Fatalf("mkdir votes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(votesDir, "A.txt"), []byte("VOTE: B\n"), 0o644); err != nil {
		t.Fatalf("write A ballot: %v", err)
	}

	ballots, err := RunBallot(context.Background(), BallotConfig{
		Session:    s,
		Experts:    labeled,
		Nonce:      s.Nonce,
		BallotBody: "Pick one.",
	}, "q", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	// A's reused ballot should show VotedFor=B. B and C ran fresh (VOTE: A).
	byVoter := map[string]string{}
	for _, b := range ballots {
		byVoter[b.VoterLabel] = b.VotedFor
	}
	if byVoter["A"] != "B" {
		t.Errorf("A VotedFor = %q, want B (reused pre-seeded file)", byVoter["A"])
	}
	// 2 fresh subprocess spawns (B + C), not 3.
	if got := atomic.LoadInt64(&spawns); got != 2 {
		t.Errorf("ballot spawns = %d, want 2 (A reused)", got)
	}
}

// TestRunBallot_ResumeSkipsMalformedCache verifies the cache predicate's
// tightness: a pre-seeded ballot file that does NOT parse to a valid active
// vote is overwritten by the fresh subprocess run.
func TestRunBallot_ResumeSkipsMalformedCache(t *testing.T) {
	var spawns int64
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			atomic.AddInt64(&spawns, 1)
			return "VOTE: A\n", nil
		},
	}
	s, _, labeled := setupRoundTest(t, "aabbccddeeff0011", exec)

	// Pre-seed A's ballot with junk that does not match VOTE: <A-Z>.
	votesDir := filepath.Join(s.Path, "voting", "votes")
	if err := os.MkdirAll(votesDir, 0o755); err != nil {
		t.Fatalf("mkdir votes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(votesDir, "A.txt"), []byte("not a ballot\n"), 0o644); err != nil {
		t.Fatalf("write A ballot: %v", err)
	}

	ballots, err := RunBallot(context.Background(), BallotConfig{
		Session:    s,
		Experts:    labeled,
		Nonce:      s.Nonce,
		BallotBody: "Pick one.",
	}, "q", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	byVoter := map[string]string{}
	for _, b := range ballots {
		byVoter[b.VoterLabel] = b.VotedFor
	}
	if byVoter["A"] != "A" {
		t.Errorf("A VotedFor = %q, want A (malformed cache should be replaced)", byVoter["A"])
	}
	// All 3 fresh spawns — malformed cache re-runs.
	if got := atomic.LoadInt64(&spawns); got != 3 {
		t.Errorf("ballot spawns = %d, want 3", got)
	}
}

// TestReadCompletedStage verifies the helper's preconditions: .done must be
// present AND output.md must be readable for the shortcut to activate.
func TestReadCompletedStage(t *testing.T) {
	dir := t.TempDir()

	// No .done, no output: not completed.
	if _, ok := readCompletedStage(dir); ok {
		t.Errorf("readCompletedStage on empty dir = ok, want false")
	}

	// output.md only (no .done): not completed.
	if err := os.WriteFile(filepath.Join(dir, "output.md"), []byte("body"), 0o644); err != nil {
		t.Fatalf("write output.md: %v", err)
	}
	if _, ok := readCompletedStage(dir); ok {
		t.Errorf("readCompletedStage with output only = ok, want false (missing .done)")
	}

	// Add .done: now completed.
	if err := os.WriteFile(filepath.Join(dir, ".done"), nil, 0o644); err != nil {
		t.Fatalf("touch .done: %v", err)
	}
	body, ok := readCompletedStage(dir)
	if !ok {
		t.Errorf("readCompletedStage with .done+output = !ok, want ok")
	}
	if body != "body" {
		t.Errorf("body = %q, want %q", body, "body")
	}

	// .done but output.md removed: fall back to no-op.
	if err := os.Remove(filepath.Join(dir, "output.md")); err != nil {
		t.Fatalf("remove output.md: %v", err)
	}
	if _, ok := readCompletedStage(dir); ok {
		t.Errorf("readCompletedStage with only .done = ok, want false (output.md missing)")
	}
}

// exec interface smoke — ensure the resume helpers don't change the failure
// semantics when the executor errors: a fresh run with no pre-seeded state
// still surfaces ErrQuorumFailedR1 on total failure. Guards against a
// refactor-regression where the resume shortcut wrongly masks a real failure.
func TestRunRound1_ResumeGuardStillReturnsQuorumFail(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "", errors.New("all fail")
		},
	}
	s, _, labeled := setupRoundTest(t, "0f0f0f0f0f0f0f0f", exec)
	_, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     2,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if !errors.Is(err, ErrQuorumFailedR1) {
		t.Fatalf("err = %v, want ErrQuorumFailedR1 (no pre-seeded state)", err)
	}
}
