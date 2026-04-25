package debate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/runner"
)

// limitErrFor builds a representative *runner.LimitError stamped with the
// given tool/pattern/help text. Helper exists to keep the test bodies short
// when a case needs different markers per expert.
func limitErrFor(tool, pattern, help string) *runner.LimitError {
	return &runner.LimitError{Tool: tool, Pattern: pattern, HelpCmd: help}
}

// TestRunRound1_LimitErrorClassifiedAsFailure exercises the new ADR-0013
// classification: an expert returning *runner.LimitError surfaces as a
// "failed" survivor with RoundOutput.LimitErr populated. Quorum=1 with two
// healthy experts means the round still succeeds (no error returned).
func TestRunRound1_LimitErrorClassifiedAsFailure(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "C" {
				return "", limitErrFor("claude-code", "anthropic rate limit", "claude /usage")
			}
			return "ok from " + label + "\n", nil
		},
	}
	s, _, labeled := setupRoundTest(t, "5151515151515151", exec)

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     1,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1: unexpected err %v (quorum met despite rate-limit)", err)
	}
	c := byLabel(outs, "C")
	if c == nil {
		t.Fatalf("label C missing from outs")
	}
	if c.Participation != "failed" {
		t.Errorf("C participation = %q, want failed", c.Participation)
	}
	if c.LimitErr == nil {
		t.Fatalf("C LimitErr nil, want populated")
	}
	if c.LimitErr.Tool != "claude-code" {
		t.Errorf("C LimitErr.Tool = %q, want claude-code", c.LimitErr.Tool)
	}
	if c.LimitErr.Pattern != "anthropic rate limit" {
		t.Errorf("C LimitErr.Pattern = %q", c.LimitErr.Pattern)
	}
}

// TestRunRound1_QuorumMetWithRateLimits asserts that even when half the
// experts hit rate-limits, surviving the quorum threshold means RunRound1
// returns nil — the orchestrator can still produce a verdict, with the
// rate-limit info attached to the failed RoundOutputs for verdict.json.
func TestRunRound1_QuorumMetWithRateLimits(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "C" {
				return "", limitErrFor("codex", "you've hit your usage limit", "codex /status")
			}
			return "answer " + label + "\n", nil
		},
	}
	s, _, labeled := setupRoundTest(t, "abcabcabcabcabca", exec)

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     2,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}
	limited := 0
	for _, o := range outs {
		if o.LimitErr != nil {
			limited++
		}
	}
	if limited != 1 {
		t.Fatalf("rate-limited count = %d, want 1", limited)
	}
}

// TestRunRound1_ErrRateLimitQuorumFail covers the new sentinel: every expert
// fails with a *LimitError, quorum=2 (unsatisfiable), so the round returns
// ErrRateLimitQuorumFail instead of ErrQuorumFailedR1. The two sentinels are
// disjoint so cmd/council can route to exit 6.
func TestRunRound1_ErrRateLimitQuorumFail(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "", limitErrFor("gemini-cli", "RESOURCE_EXHAUSTED", "check https://aistudio.google.com/apikey for quota and billing"+label)
		},
	}
	s, _, labeled := setupRoundTest(t, "f1f2f3f4f5f6f7f8", exec)

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     2,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if !errors.Is(err, ErrRateLimitQuorumFail) {
		t.Fatalf("err = %v, want ErrRateLimitQuorumFail", err)
	}
	if errors.Is(err, ErrQuorumFailedR1) {
		t.Errorf("err matches ErrQuorumFailedR1; sentinels must be disjoint")
	}
	if errors.Is(err, ErrQuorumFailedR2) {
		t.Errorf("err matches ErrQuorumFailedR2; sentinels must be disjoint")
	}
	if len(outs) != 3 {
		t.Fatalf("len(outs) = %d, want 3", len(outs))
	}
	for _, o := range outs {
		if o.LimitErr == nil {
			t.Errorf("label %s LimitErr nil, want populated", o.Label)
		}
	}
}

// TestRunRound1_NoLimitErrUsesGenericSentinel asserts the disjoint contract
// in the other direction: when no expert returned a LimitError, the original
// ErrQuorumFailedR1 sentinel surfaces. Without this, a regression that
// mis-classified plain failures as rate-limited would silently re-route exit
// codes and break operator dashboards.
func TestRunRound1_NoLimitErrUsesGenericSentinel(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "", errors.New("plain non-rate-limit failure")
		},
	}
	s, _, labeled := setupRoundTest(t, "9999888877776666", exec)

	_, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     2,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if !errors.Is(err, ErrQuorumFailedR1) {
		t.Fatalf("err = %v, want ErrQuorumFailedR1", err)
	}
	if errors.Is(err, ErrRateLimitQuorumFail) {
		t.Errorf("err matches ErrRateLimitQuorumFail with no LimitErr present")
	}
}

// TestRunBallot_LimitErrorClassified covers the ballot-stage analogue of
// the round classification: a voter subprocess returning *runner.LimitError
// surfaces as a discarded ballot (VotedFor="") with Ballot.LimitErr
// populated. Ballots run with AllowedTools=nil so this is rare in practice,
// but the orchestrator still records it in verdict.json's rate_limits[].
func TestRunBallot_LimitErrorClassified(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(req.StdoutFile)
			if label == "C.txt" {
				return "", limitErrFor("gemini-cli", "QUOTA_EXHAUSTED", "check https://aistudio.google.com/apikey for quota and billing")
			}
			return "VOTE: A\n", nil
		},
	}
	cfg, labeled := setupBallotTest(t, "0bad0bad0bad0bad", exec)
	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	var c *Ballot
	for i := range ballots {
		if ballots[i].VoterLabel == "C" {
			c = &ballots[i]
			break
		}
	}
	if c == nil {
		t.Fatalf("ballot for C missing; len=%d, labeled=%d", len(ballots), len(labeled))
	}
	if c.VotedFor != "" {
		t.Errorf("C VotedFor = %q, want empty (discarded)", c.VotedFor)
	}
	if c.LimitErr == nil {
		t.Fatalf("C LimitErr nil, want populated")
	}
	if c.LimitErr.Tool != "gemini-cli" {
		t.Errorf("C LimitErr.Tool = %q, want gemini-cli", c.LimitErr.Tool)
	}
}

// TestRunRound2_ErrRateLimitQuorumFail mirrors TestRunRound1_ErrRateLimitQuorumFail
// for the R2 path. Because R2 carry-forward preserves participation when R1
// succeeded, we use a configuration where R1 already dropped enough experts
// to leave the R2 cohort short of quorum once the surviving expert
// rate-limits its R2 attempt.
func TestRunRound2_ErrRateLimitQuorumFail(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "", limitErrFor("codex", "exceeded retry limit, last status: 429", "codex /status")
		},
	}
	s, _, labeled := setupRoundTest(t, "a1a2a3a4a5a6a7a8", exec)

	// Build an R1 result where only label "A" reached R2; B and C are
	// already-failed R1 entries (no carry-forward, no R2 spawn).
	r1 := []RoundOutput{
		{Label: "A", Name: "alpha", Participation: "ok", Body: "r1-A"},
		{Label: "B", Name: "bravo", Participation: "failed"},
		{Label: "C", Name: "charlie", Participation: "failed"},
	}

	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       2,
		MaxRetries:   0,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if !errors.Is(err, ErrRateLimitQuorumFail) {
		t.Fatalf("err = %v, want ErrRateLimitQuorumFail", err)
	}
	if errors.Is(err, ErrQuorumFailedR2) {
		t.Errorf("err matches ErrQuorumFailedR2; sentinels must be disjoint")
	}
	a := byLabel(outs, "A")
	if a == nil {
		t.Fatalf("label A missing from outs")
	}
	// Carry-forward overwrites R2 with R1 body, but LimitErr is set
	// on the failed R2 attempt — verifies the orchestrator can still
	// see the rate-limit even when R2 carries forward to "carried".
	if a.LimitErr == nil {
		t.Errorf("A LimitErr nil after R2 rate-limit; want populated")
	}
}
