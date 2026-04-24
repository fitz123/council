package debate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/session"
)

// safeStrings is a tiny concurrency-safe append-only string slice used by
// capture-the-prompt tests. It exists here (not in a shared helper) because
// it has a single caller and keeps the test file self-contained.
type safeStrings struct {
	mu sync.Mutex
	s  []string
}

func (p *safeStrings) Add(v string) {
	p.mu.Lock()
	p.s = append(p.s, v)
	p.mu.Unlock()
}

func (p *safeStrings) List() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.s))
	copy(out, p.s)
	return out
}

// setupBallotTest reuses setupRoundTest's scaffolding and, in addition,
// returns a minimal BallotConfig pointing at the same active experts. The
// caller supplies the executor (registered under testExecName).
func setupBallotTest(t *testing.T, nonce string, exec executor.Executor) (BallotConfig, []LabeledExpert) {
	t.Helper()
	s, prof, labeled := setupRoundTest(t, nonce, exec)
	cfg := BallotConfig{
		Session:    s,
		Experts:    labeled,
		Nonce:      s.Nonce,
		BallotBody: prof.Voting.BallotPromptBody,
		Timeout:    prof.Voting.Timeout,
		MaxRetries: prof.MaxRetries,
	}
	return cfg, labeled
}

// activeLabelsOf returns the sorted label slice for a set of labeled experts.
func activeLabelsOf(labeled []LabeledExpert) []string {
	labels := make([]string, len(labeled))
	for i, ex := range labeled {
		labels[i] = ex.Label
	}
	sort.Strings(labels)
	return labels
}

func TestRunBallot_AllValidVotes(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			// Each voter votes for A (label of first expert in the active set).
			return "VOTE: A\n", nil
		},
	}
	cfg, labeled := setupBallotTest(t, "0123456789abcdef", exec)

	ballots, err := RunBallot(context.Background(), cfg, "q?", "aggregate body")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	if len(ballots) != len(labeled) {
		t.Fatalf("len(ballots) = %d, want %d", len(ballots), len(labeled))
	}
	for _, b := range ballots {
		if b.VotedFor != "A" {
			t.Errorf("voter %s voted %q, want A", b.VoterLabel, b.VotedFor)
		}
	}
	// Per-voter artifacts present.
	for _, b := range ballots {
		path := filepath.Join(cfg.Session.Path, "voting", "votes", b.VoterLabel+".txt")
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("voter %s: missing vote file: %v", b.VoterLabel, rerr)
			continue
		}
		if !strings.Contains(string(body), "VOTE: A") {
			t.Errorf("voter %s: vote file = %q, missing VOTE line", b.VoterLabel, body)
		}
	}
	// Results sorted by label.
	for i := 1; i < len(ballots); i++ {
		if ballots[i-1].VoterLabel >= ballots[i].VoterLabel {
			t.Errorf("ballots not sorted: %v", ballots)
		}
	}
}

func TestRunBallot_MalformedDiscarded(t *testing.T) {
	// Voter A produces garbage (no VOTE line); B and C vote cleanly.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(req.StdoutFile) // e.g. "A.txt"
			label = strings.TrimSuffix(label, ".txt")
			if label == "A" {
				return "I don't know. Maybe A? Maybe B?\n", nil
			}
			return "VOTE: B\n", nil
		},
	}
	cfg, _ := setupBallotTest(t, "1111222233334444", exec)
	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	got := map[string]string{}
	for _, b := range ballots {
		got[b.VoterLabel] = b.VotedFor
	}
	if got["A"] != "" {
		t.Errorf("A (malformed) voted %q, want discarded (\"\")", got["A"])
	}
	if got["B"] != "B" || got["C"] != "B" {
		t.Errorf("B/C votes = %v, want both \"B\"", got)
	}
}

func TestRunBallot_MultipleVoteLinesDiscarded(t *testing.T) {
	// A voter that emits more than one VOTE: line is malformed per D8:
	// we refuse to guess whether the voter reconsidered (second wins)
	// or quoted the prompt (first wins). Both ballots must be discarded.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := strings.TrimSuffix(filepath.Base(req.StdoutFile), ".txt")
			if label == "A" {
				// Two distinct votes on separate lines.
				return "VOTE: A\nOn reflection:\nVOTE: B\n", nil
			}
			if label == "B" {
				// Duplicate of the same vote also counts as multi-vote.
				return "VOTE: C\nVOTE: C\n", nil
			}
			return "VOTE: C\n", nil
		},
	}
	cfg, _ := setupBallotTest(t, "1111222233334444", exec)
	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	got := map[string]string{}
	for _, b := range ballots {
		got[b.VoterLabel] = b.VotedFor
	}
	if got["A"] != "" {
		t.Errorf("A (two distinct VOTE lines) = %q, want discarded", got["A"])
	}
	if got["B"] != "" {
		t.Errorf("B (duplicate VOTE lines) = %q, want discarded", got["B"])
	}
	if got["C"] != "C" {
		t.Errorf("C = %q, want \"C\"", got["C"])
	}
}

func TestRunBallot_VoteForInactiveLabelDiscarded(t *testing.T) {
	// Voter A votes for D — not in the active set (only A/B/C). Discarded.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := strings.TrimSuffix(filepath.Base(req.StdoutFile), ".txt")
			if label == "A" {
				return "VOTE: D\n", nil
			}
			return "VOTE: A\n", nil
		},
	}
	cfg, _ := setupBallotTest(t, "aaaabbbbccccdddd", exec)
	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	got := map[string]string{}
	for _, b := range ballots {
		got[b.VoterLabel] = b.VotedFor
	}
	if got["A"] != "" {
		t.Errorf("A (out-of-set vote) = %q, want discarded", got["A"])
	}
	if got["B"] != "A" || got["C"] != "A" {
		t.Errorf("B/C votes = %v, want both A", got)
	}
}

func TestRunBallot_FreshSubprocess_NoRoleBody(t *testing.T) {
	// Fresh subprocess: the voter sees only the ballot body + question +
	// candidates — NOT the voter's own role body ("you are alpha").
	var capturedPrompts safeStrings
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			capturedPrompts.Add(req.Prompt)
			return "VOTE: A\n", nil
		},
	}
	nonce := "cafebabedeadbeef"
	cfg, _ := setupBallotTest(t, nonce, exec)
	_, err := RunBallot(context.Background(), cfg, "q?", "aggregate body")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	// Per ADR-0011, every structural fence in the ballot prompt carries the
	// session nonce so the tightened forgery regex
	// (`^=== .*\[nonce-[0-9a-f]{16}\] ===[ \t\r]*$` in pkg/prompt/injection.go,
	// landed in Task 3) can reject forged fences without over-rejecting
	// benign web content.
	wantFences := []string{
		"=== USER QUESTION (untrusted input) [nonce-" + nonce + "] ===",
		"=== END USER QUESTION [nonce-" + nonce + "] ===",
		"=== CANDIDATES [nonce-" + nonce + "] ===",
		"=== END CANDIDATES [nonce-" + nonce + "] ===",
	}
	for _, p := range capturedPrompts.List() {
		for _, role := range []string{"you are alpha", "you are bravo", "you are charlie"} {
			if strings.Contains(p, role) {
				t.Errorf("ballot prompt leaked role body %q:\n%s", role, p)
			}
		}
		for _, fence := range wantFences {
			if !strings.Contains(p, fence) {
				t.Errorf("ballot prompt missing fence %q:\n%s", fence, p)
			}
		}
		if !strings.Contains(p, "aggregate body") {
			t.Errorf("ballot prompt missing aggregate content:\n%s", p)
		}
		if !strings.Contains(p, "VOTE: <label>") {
			t.Errorf("ballot prompt missing VOTE instruction:\n%s", p)
		}
	}
}

// TestRunBallot_AlwaysToolsOff verifies every ballot subprocess is spawned
// with executor.Request.AllowedTools == nil and PermissionMode == "" (F17).
// Voting is hardcoded tools-off regardless of how R1/R2 expert spawns are
// configured — adjudication, not research.
func TestRunBallot_AlwaysToolsOff(t *testing.T) {
	var mu sync.Mutex
	var seen []executor.Request
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			return "VOTE: A\n", nil
		},
	}
	cfg, labeled := setupBallotTest(t, "0123456789abcdef", exec)
	if _, err := RunBallot(context.Background(), cfg, "q?", "agg"); err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(labeled) {
		t.Fatalf("captured %d ballot requests, want %d", len(seen), len(labeled))
	}
	for _, req := range seen {
		if req.AllowedTools != nil {
			t.Errorf("ballot %s: AllowedTools = %v, want nil (ballots always tools-off)", req.StdoutFile, req.AllowedTools)
		}
		if req.PermissionMode != "" {
			t.Errorf("ballot %s: PermissionMode = %q, want \"\"", req.StdoutFile, req.PermissionMode)
		}
	}
}

func TestRunBallot_SubprocessFailureDiscarded(t *testing.T) {
	// B's subprocess fails after retries — ballot must be discarded without
	// aborting the run.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := strings.TrimSuffix(filepath.Base(req.StdoutFile), ".txt")
			if label == "B" {
				return "", errors.New("B subprocess fails")
			}
			return "VOTE: A\n", nil
		},
	}
	cfg, _ := setupBallotTest(t, "3333444455556666", exec)
	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot must not error on per-voter failure: %v", err)
	}
	got := map[string]string{}
	for _, b := range ballots {
		got[b.VoterLabel] = b.VotedFor
	}
	if got["B"] != "" {
		t.Errorf("B (subprocess failure) = %q, want discarded", got["B"])
	}
}

func TestRunBallot_NonceLeakageDiscarded(t *testing.T) {
	// A voter that echoes the session nonce back has its ballot discarded
	// (CheckForgery fires), even if the output also contains a valid VOTE
	// line — defense in depth per ADR-0008.
	nonce := "deadbeefdeadbeef"
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := strings.TrimSuffix(filepath.Base(req.StdoutFile), ".txt")
			if label == "A" {
				return "leaked: " + nonce + "\nVOTE: B\n", nil
			}
			return "VOTE: B\n", nil
		},
	}
	cfg, _ := setupBallotTest(t, nonce, exec)
	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}
	got := map[string]string{}
	for _, b := range ballots {
		got[b.VoterLabel] = b.VotedFor
	}
	if got["A"] != "" {
		t.Errorf("A (nonce leak) = %q, want discarded", got["A"])
	}
	if got["B"] != "B" || got["C"] != "B" {
		t.Errorf("B/C votes = %v, want both B", got)
	}
}

func TestRunBallot_NilSession(t *testing.T) {
	_, err := RunBallot(context.Background(), BallotConfig{Session: nil}, "q", "agg")
	if err == nil {
		t.Fatalf("expected error for nil session")
	}
}

func TestTally_UniqueWinner_ThreeZeroZero(t *testing.T) {
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "A"},
		{VoterLabel: "B", VotedFor: "A"},
		{VoterLabel: "C", VotedFor: "A"},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "A" {
		t.Errorf("Winner = %q, want A", r.Winner)
	}
	if r.TiedCandidates != nil {
		t.Errorf("TiedCandidates = %v, want nil", r.TiedCandidates)
	}
	if r.Votes["A"] != 3 || r.Votes["B"] != 0 || r.Votes["C"] != 0 {
		t.Errorf("Votes = %v, want {A:3, B:0, C:0}", r.Votes)
	}
}

func TestTally_UniqueWinner_TwoOneZero(t *testing.T) {
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "B"},
		{VoterLabel: "B", VotedFor: "A"},
		{VoterLabel: "C", VotedFor: "A"},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "A" {
		t.Errorf("Winner = %q, want A", r.Winner)
	}
	if r.TiedCandidates != nil {
		t.Errorf("TiedCandidates = %v, want nil", r.TiedCandidates)
	}
	if r.Votes["A"] != 2 || r.Votes["B"] != 1 || r.Votes["C"] != 0 {
		t.Errorf("Votes = %v, want {A:2, B:1, C:0}", r.Votes)
	}
}

func TestTally_ThreeWayTie(t *testing.T) {
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "A"},
		{VoterLabel: "B", VotedFor: "B"},
		{VoterLabel: "C", VotedFor: "C"},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "" {
		t.Errorf("Winner = %q, want empty", r.Winner)
	}
	want := []string{"A", "B", "C"}
	if !sliceEq(r.TiedCandidates, want) {
		t.Errorf("TiedCandidates = %v, want %v", r.TiedCandidates, want)
	}
}

func TestTally_ReducedCohort_TwoZeroUniqueWinner(t *testing.T) {
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "A"},
		{VoterLabel: "B", VotedFor: "A"},
	}
	r := Tally(ballots, []string{"A", "B"})
	if r.Winner != "A" {
		t.Errorf("Winner = %q, want A", r.Winner)
	}
	if r.TiedCandidates != nil {
		t.Errorf("TiedCandidates = %v, want nil", r.TiedCandidates)
	}
}

func TestTally_ReducedCohort_OneOneTie(t *testing.T) {
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "A"},
		{VoterLabel: "B", VotedFor: "B"},
	}
	r := Tally(ballots, []string{"A", "B"})
	if r.Winner != "" {
		t.Errorf("Winner = %q, want empty", r.Winner)
	}
	want := []string{"A", "B"}
	if !sliceEq(r.TiedCandidates, want) {
		t.Errorf("TiedCandidates = %v, want %v", r.TiedCandidates, want)
	}
}

func TestTally_MalformedBallotsDiscarded_SplitIsTie(t *testing.T) {
	// 3 voters, 1 malformed, remaining 2 split 1-1 → tie between the two
	// receiving labels. C receives no votes (still tied at max? no — A and B
	// are at 1, C is at 0, so max=1 and tied=[A, B]).
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: ""},
		{VoterLabel: "B", VotedFor: "A"},
		{VoterLabel: "C", VotedFor: "B"},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "" {
		t.Errorf("Winner = %q, want empty (1-1-0 among valid)", r.Winner)
	}
	want := []string{"A", "B"}
	if !sliceEq(r.TiedCandidates, want) {
		t.Errorf("TiedCandidates = %v, want %v", r.TiedCandidates, want)
	}
}

func TestTally_MalformedBallotsDiscarded_AgreeIsWinner(t *testing.T) {
	// 3 voters, 1 malformed, remaining 2 agree on A → unique winner A.
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: ""},
		{VoterLabel: "B", VotedFor: "A"},
		{VoterLabel: "C", VotedFor: "A"},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "A" {
		t.Errorf("Winner = %q, want A", r.Winner)
	}
}

func TestTally_AllMalformed_EveryLabelTied(t *testing.T) {
	// Degenerate case: every ballot discarded → votes all zero → every
	// active label is tied at max=0 per D8.
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: ""},
		{VoterLabel: "B", VotedFor: ""},
		{VoterLabel: "C", VotedFor: ""},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "" {
		t.Errorf("Winner = %q, want empty", r.Winner)
	}
	want := []string{"A", "B", "C"}
	if !sliceEq(r.TiedCandidates, want) {
		t.Errorf("TiedCandidates = %v, want %v", r.TiedCandidates, want)
	}
	for _, l := range want {
		if r.Votes[l] != 0 {
			t.Errorf("Votes[%s] = %d, want 0", l, r.Votes[l])
		}
	}
}

func TestTally_SingleActive_AllMalformedIsNoConsensus(t *testing.T) {
	// Reduced cohort of size 1 with a malformed sole ballot. Per D8's
	// degenerate rule ("all ballots malformed → every active label tied at
	// max zero"), this must surface TiedCandidates=[A], NOT Winner=A. The
	// literal "unique max → winner" read of the rule would elect A with zero
	// votes — the design deliberately carves this out as no_consensus.
	ballots := []Ballot{{VoterLabel: "A", VotedFor: ""}}
	r := Tally(ballots, []string{"A"})
	if r.Winner != "" {
		t.Errorf("Winner = %q, want empty (zero valid ballots cannot elect)", r.Winner)
	}
	want := []string{"A"}
	if !sliceEq(r.TiedCandidates, want) {
		t.Errorf("TiedCandidates = %v, want %v", r.TiedCandidates, want)
	}
	if r.Votes["A"] != 0 {
		t.Errorf("Votes[A] = %d, want 0", r.Votes["A"])
	}
}

func TestTally_TiedOnlyContainsActiveLabels(t *testing.T) {
	// Ballots vote for D which is not in active set; D must not appear in
	// the result even though it receives "votes".
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "D"},
		{VoterLabel: "B", VotedFor: "A"},
		{VoterLabel: "C", VotedFor: "D"},
	}
	r := Tally(ballots, []string{"A", "B", "C"})
	if r.Winner != "A" {
		t.Errorf("Winner = %q, want A (A is the only valid vote target)", r.Winner)
	}
	if _, ok := r.Votes["D"]; ok {
		t.Errorf("Votes must not include inactive label D: %v", r.Votes)
	}
}

func TestTally_TiedCandidatesAreActiveAndMax(t *testing.T) {
	// Invariant: if TiedCandidates != nil, every listed label is active and
	// has the max vote count.
	ballots := []Ballot{
		{VoterLabel: "A", VotedFor: "A"},
		{VoterLabel: "B", VotedFor: "A"},
		{VoterLabel: "C", VotedFor: "B"},
		{VoterLabel: "D", VotedFor: "B"},
	}
	active := []string{"A", "B", "C", "D"}
	r := Tally(ballots, active)
	if r.Winner != "" {
		t.Errorf("Winner = %q, want empty (A and B tied at 2)", r.Winner)
	}
	max := 0
	for _, l := range active {
		if r.Votes[l] > max {
			max = r.Votes[l]
		}
	}
	activeSet := map[string]bool{}
	for _, l := range active {
		activeSet[l] = true
	}
	for _, l := range r.TiedCandidates {
		if !activeSet[l] {
			t.Errorf("tied label %s not in active set", l)
		}
		if r.Votes[l] != max {
			t.Errorf("tied label %s has %d votes, max = %d", l, r.Votes[l], max)
		}
	}
}

func TestSelectOutput_UniqueWinner_CopiesToOutputMd(t *testing.T) {
	s, _, _ := setupRoundTest(t, "5555666677778888", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	r2 := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "A final answer\n"},
		{Label: "B", Participation: "ok", Body: "B final answer\n"},
		{Label: "C", Participation: "ok", Body: "C final answer\n"},
	}
	result := TallyResult{
		Votes:   map[string]int{"A": 0, "B": 3, "C": 0},
		Winner:  "B",
		Ballots: []Ballot{{VoterLabel: "A", VotedFor: "B"}, {VoterLabel: "B", VotedFor: "B"}, {VoterLabel: "C", VotedFor: "B"}},
	}
	if err := SelectOutput(s, result, r2); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(s.Path, "output.md"))
	if err != nil {
		t.Fatalf("read output.md: %v", err)
	}
	if string(body) != "B final answer\n" {
		t.Errorf("output.md = %q, want B's body", body)
	}
	// Per-label files must NOT exist in the unique-winner case.
	for _, l := range []string{"A", "B", "C"} {
		if _, err := os.Stat(filepath.Join(s.Path, "output-"+l+".md")); !os.IsNotExist(err) {
			t.Errorf("output-%s.md must not exist in unique-winner case", l)
		}
	}
	// tally.json present + shape.
	raw, err := os.ReadFile(filepath.Join(s.Path, "voting", "tally.json"))
	if err != nil {
		t.Fatalf("read tally.json: %v", err)
	}
	var got struct {
		Votes          map[string]int `json:"votes"`
		Winner         string         `json:"winner"`
		TiedCandidates []string       `json:"tied_candidates"`
		Ballots        []struct {
			VoterLabel string `json:"voter_label"`
			VotedFor   string `json:"voted_for"`
		} `json:"ballots"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal tally.json: %v\n%s", err, raw)
	}
	if got.Winner != "B" {
		t.Errorf("tally.winner = %q, want B", got.Winner)
	}
	if got.TiedCandidates != nil {
		t.Errorf("tally.tied_candidates = %v, want null", got.TiedCandidates)
	}
	if len(got.Ballots) != 3 {
		t.Errorf("tally.ballots len = %d, want 3", len(got.Ballots))
	}
}

func TestSelectOutput_ThreeWayTie_CopiesPerLabel(t *testing.T) {
	s, _, _ := setupRoundTest(t, "9999aaaabbbbcccc", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	r2 := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "A\n"},
		{Label: "B", Participation: "ok", Body: "B\n"},
		{Label: "C", Participation: "ok", Body: "C\n"},
	}
	result := TallyResult{
		Votes:          map[string]int{"A": 1, "B": 1, "C": 1},
		TiedCandidates: []string{"A", "B", "C"},
		Ballots: []Ballot{
			{VoterLabel: "A", VotedFor: "A"},
			{VoterLabel: "B", VotedFor: "B"},
			{VoterLabel: "C", VotedFor: "C"},
		},
	}
	if err := SelectOutput(s, result, r2); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}
	for _, l := range []string{"A", "B", "C"} {
		body, err := os.ReadFile(filepath.Join(s.Path, "output-"+l+".md"))
		if err != nil {
			t.Fatalf("read output-%s.md: %v", l, err)
		}
		if string(body) != l+"\n" {
			t.Errorf("output-%s.md = %q, want %q", l, body, l+"\n")
		}
	}
	// Single output.md must NOT exist in the tie case.
	if _, err := os.Stat(filepath.Join(s.Path, "output.md")); !os.IsNotExist(err) {
		t.Errorf("output.md must not exist in tie case")
	}
}

func TestSelectOutput_TwoWayTie_N2(t *testing.T) {
	s, _, _ := setupRoundTest(t, "ddddeeeeffff0000", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	r2 := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "A body\n"},
		{Label: "B", Participation: "ok", Body: "B body\n"},
	}
	result := TallyResult{
		Votes:          map[string]int{"A": 1, "B": 1},
		TiedCandidates: []string{"A", "B"},
		Ballots: []Ballot{
			{VoterLabel: "A", VotedFor: "A"},
			{VoterLabel: "B", VotedFor: "B"},
		},
	}
	if err := SelectOutput(s, result, r2); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}
	for _, l := range []string{"A", "B"} {
		path := filepath.Join(s.Path, "output-"+l+".md")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("output-%s.md missing: %v", l, err)
		}
	}
}

// TestSelectOutput_Resume_TieToWinner_CleansStaleTiedOutputs covers the
// resume case where a first invocation produced a tie (output-A.md +
// output-B.md) but a re-run produced a unique winner. The new output.md
// must land AND the prior output-*.md files must be removed so the on-disk
// artifacts agree with tally.json.
func TestSelectOutput_Resume_TieToWinner_CleansStaleTiedOutputs(t *testing.T) {
	s, _, _ := setupRoundTest(t, "1234567890abcdef", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	if err := os.WriteFile(filepath.Join(s.Path, "output-A.md"), []byte("stale A\n"), 0o644); err != nil {
		t.Fatalf("seed output-A.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.Path, "output-B.md"), []byte("stale B\n"), 0o644); err != nil {
		t.Fatalf("seed output-B.md: %v", err)
	}
	r2 := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "A body\n"},
		{Label: "B", Participation: "ok", Body: "B body\n"},
	}
	result := TallyResult{
		Votes:  map[string]int{"A": 2, "B": 0},
		Winner: "A",
	}
	if err := SelectOutput(s, result, r2); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(s.Path, "output.md"))
	if err != nil {
		t.Fatalf("read output.md: %v", err)
	}
	if string(body) != "A body\n" {
		t.Errorf("output.md = %q, want winner body", body)
	}
	for _, l := range []string{"A", "B"} {
		if _, err := os.Stat(filepath.Join(s.Path, "output-"+l+".md")); !os.IsNotExist(err) {
			t.Errorf("output-%s.md must be removed after winner selection", l)
		}
	}
}

// TestSelectOutput_Resume_WinnerToTie_CleansStaleOutputMd covers the
// reverse direction: a first invocation wrote output.md (winner) but the
// re-run produced a tie. output.md must vanish and the new output-<label>.md
// files must land.
func TestSelectOutput_Resume_WinnerToTie_CleansStaleOutputMd(t *testing.T) {
	s, _, _ := setupRoundTest(t, "abcdef0123456789", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	if err := os.WriteFile(filepath.Join(s.Path, "output.md"), []byte("stale winner\n"), 0o644); err != nil {
		t.Fatalf("seed output.md: %v", err)
	}
	r2 := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "A body\n"},
		{Label: "B", Participation: "ok", Body: "B body\n"},
	}
	result := TallyResult{
		Votes:          map[string]int{"A": 1, "B": 1},
		TiedCandidates: []string{"A", "B"},
	}
	if err := SelectOutput(s, result, r2); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Path, "output.md")); !os.IsNotExist(err) {
		t.Errorf("output.md must be removed after tie selection")
	}
	for _, l := range []string{"A", "B"} {
		body, err := os.ReadFile(filepath.Join(s.Path, "output-"+l+".md"))
		if err != nil {
			t.Fatalf("read output-%s.md: %v", l, err)
		}
		if string(body) != l+" body\n" {
			t.Errorf("output-%s.md = %q, want %q body", l, body, l)
		}
	}
}

// TestSelectOutput_Resume_TiedSetShrinks_CleansDroppedLabel covers a third
// resume scenario: first run produced a 3-way tie (output-A/B/C.md), the
// re-run reduced the tied set to 2 (e.g. one previously-malformed ballot
// now votes). The label dropped from the tied set must have its output
// file removed so the artifacts match tally.json.
func TestSelectOutput_Resume_TiedSetShrinks_CleansDroppedLabel(t *testing.T) {
	s, _, _ := setupRoundTest(t, "fedcba9876543210", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	for _, l := range []string{"A", "B", "C"} {
		if err := os.WriteFile(filepath.Join(s.Path, "output-"+l+".md"), []byte("stale "+l+"\n"), 0o644); err != nil {
			t.Fatalf("seed output-%s.md: %v", l, err)
		}
	}
	r2 := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "A body\n"},
		{Label: "B", Participation: "ok", Body: "B body\n"},
		{Label: "C", Participation: "ok", Body: "C body\n"},
	}
	result := TallyResult{
		Votes:          map[string]int{"A": 1, "B": 1, "C": 0},
		TiedCandidates: []string{"A", "B"},
	}
	if err := SelectOutput(s, result, r2); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Path, "output-C.md")); !os.IsNotExist(err) {
		t.Errorf("output-C.md must be removed after C dropped from tied set")
	}
	for _, l := range []string{"A", "B"} {
		body, err := os.ReadFile(filepath.Join(s.Path, "output-"+l+".md"))
		if err != nil {
			t.Fatalf("read output-%s.md: %v", l, err)
		}
		if string(body) != l+" body\n" {
			t.Errorf("output-%s.md = %q, want fresh %q body", l, body, l)
		}
	}
}

// TestCleanOutputs_PathWithGlobMetacharacters ensures cleanOutputs works for
// legal session paths containing filepath.Glob metacharacters (`[`, `]`,
// `*`, `?`). A prior filepath.Glob-based implementation would silently skip
// stale artifacts or return ErrBadPattern because Glob does not escape the
// literal path prefix.
func TestCleanOutputs_PathWithGlobMetacharacters(t *testing.T) {
	base := filepath.Join(t.TempDir(), "council[dev]")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	for _, name := range []string{"output.md", "output-A.md", "output-B.md"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("stale\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// A sibling file that must NOT be removed — sanity check the prefix/suffix
	// filter is not over-eager.
	if err := os.WriteFile(filepath.Join(base, "keep.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("seed keep.md: %v", err)
	}
	s := &session.Session{Path: base}
	if err := cleanOutputs(s); err != nil {
		t.Fatalf("cleanOutputs: %v", err)
	}
	for _, name := range []string{"output.md", "output-A.md", "output-B.md"} {
		if _, err := os.Stat(filepath.Join(base, name)); !os.IsNotExist(err) {
			t.Errorf("%s must be removed even when path contains glob metacharacters", name)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "keep.md")); err != nil {
		t.Errorf("keep.md must remain: %v", err)
	}
}

func TestSelectOutput_MissingR2ForWinner_Error(t *testing.T) {
	s, _, _ := setupRoundTest(t, "0000111122223333", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	result := TallyResult{Winner: "X", Votes: map[string]int{"X": 1}}
	if err := SelectOutput(s, result, []RoundOutput{{Label: "A"}}); err == nil {
		t.Fatal("expected error when winner label has no R2 output")
	}
}

func TestSelectOutput_NilSession(t *testing.T) {
	if err := SelectOutput(nil, TallyResult{Winner: "A"}, []RoundOutput{{Label: "A", Body: "x"}}); err == nil {
		t.Fatal("expected error for nil session")
	}
}

func TestActiveLabels_SortedForTally(t *testing.T) {
	// Smoke test that our helper returns a deterministic sorted slice.
	labeled := []LabeledExpert{
		{Label: "C", Role: config.RoleConfig{Name: "c"}},
		{Label: "A", Role: config.RoleConfig{Name: "a"}},
		{Label: "B", Role: config.RoleConfig{Name: "b"}},
	}
	got := activeLabelsOf(labeled)
	want := []string{"A", "B", "C"}
	if !sliceEq(got, want) {
		t.Errorf("activeLabelsOf = %v, want %v", got, want)
	}
}

// sliceEq reports whether two string slices have identical contents in order.
func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Guard against Timeout type drift if BallotConfig.Timeout is ever reshaped.
var _ time.Duration = BallotConfig{}.Timeout
