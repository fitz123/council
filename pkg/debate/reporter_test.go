package debate

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fitz123/council/pkg/executor"
)

// recordingReporter captures every OnStageDone call into an in-memory slice
// so tests can assert on the per-stage events the round/ballot fan-outs
// fire. Safe for concurrent use — the round runners spawn one goroutine per
// expert.
type recordingReporter struct {
	mu     sync.Mutex
	events []StageEvent
}

func (r *recordingReporter) OnStageDone(e StageEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingReporter) snapshot() []StageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StageEvent, len(r.events))
	copy(out, r.events)
	return out
}

func eventsByLabel(events []StageEvent, kind string) map[string]StageEvent {
	out := make(map[string]StageEvent, len(events))
	for _, e := range events {
		if e.Kind != kind {
			continue
		}
		out[e.Label] = e
	}
	return out
}

// TestReporter_RunRound1_FiresPerExpert pins the contract that runExpertR1
// fires the reporter exactly once per expert with the FINAL state set by
// the function body — not the zero "failed" state at defer registration.
// This catches a refactor that returns a fresh RoundOutput literal instead
// of mutating the named result variable.
func TestReporter_RunRound1_FiresPerExpert(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "expert body", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "deadbeef00000000", exec)
	rep := &recordingReporter{}
	cfg := RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: prof.Round2Prompt.Body,
		Reporter:     rep,
	}

	r1, err := RunRound1(context.Background(), cfg, "q?")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}

	events := rep.snapshot()
	if got, want := len(events), len(r1); got != want {
		t.Fatalf("len(events) = %d, want %d", got, want)
	}
	byLabel := eventsByLabel(events, "round-expert")
	for _, o := range r1 {
		ev, ok := byLabel[o.Label]
		if !ok {
			t.Errorf("missing event for label %s", o.Label)
			continue
		}
		if ev.Round != 1 {
			t.Errorf("label %s: Round = %d, want 1", o.Label, ev.Round)
		}
		if ev.Participation != "ok" {
			t.Errorf("label %s: Participation = %q, want ok (defer must capture FINAL state, not initial 'failed')",
				o.Label, ev.Participation)
		}
		if string(ev.Body) != "expert body" {
			t.Errorf("label %s: Body = %q, want expert body", o.Label, string(ev.Body))
		}
		if ev.Resumed {
			t.Errorf("label %s: Resumed = true on fresh run", o.Label)
		}
	}
}

// TestReporter_RunRound1_FailedFiresWithFailedState pins the contract that
// a subprocess failure still produces an event — with Participation="failed"
// and an empty Body — so the live stream surfaces every cancellation /
// crash, not just successes.
func TestReporter_RunRound1_FailedFiresWithFailedState(t *testing.T) {
	failErr := errors.New("subprocess fail")
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "", failErr
		},
	}
	s, prof, labeled := setupRoundTest(t, "0001000200030004", exec)
	rep := &recordingReporter{}
	cfg := RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
		Reporter:   rep,
	}

	_, _ = RunRound1(context.Background(), cfg, "q?") // quorum failure expected

	events := rep.snapshot()
	if len(events) == 0 {
		t.Fatal("no events; reporter must fire even on failure")
	}
	for _, ev := range events {
		if ev.Kind != "round-expert" {
			t.Errorf("unexpected kind %q", ev.Kind)
		}
		if ev.Participation != "failed" {
			t.Errorf("Participation = %q, want failed", ev.Participation)
		}
		if len(ev.Body) != 0 {
			t.Errorf("Body = %q, want empty on failure", string(ev.Body))
		}
	}
}

// TestReporter_RunBallot_FiresPerVoter pins the ballot-stage analogue:
// each voter produces one event with the parsed VotedFor reflecting the
// final state. Also confirms VotedForRealName resolves against the active
// cohort so the live stream can render "voted for A (realname)".
func TestReporter_RunBallot_FiresPerVoter(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "VOTE: A\n", nil
		},
	}
	cfg, labeled := setupBallotTest(t, "abcdef0012345678", exec)
	rep := &recordingReporter{}
	cfg.Reporter = rep

	ballots, err := RunBallot(context.Background(), cfg, "q?", "agg")
	if err != nil {
		t.Fatalf("RunBallot: %v", err)
	}

	var wantRealNameForA string
	for _, ex := range labeled {
		if ex.Label == "A" {
			wantRealNameForA = ex.Role.Name
			break
		}
	}
	if wantRealNameForA == "" {
		t.Fatal("no expert with label A in fixture")
	}

	events := rep.snapshot()
	if got, want := len(events), len(ballots); got != want {
		t.Fatalf("len(events) = %d, want %d", got, want)
	}
	byLabel := eventsByLabel(events, "ballot")
	for _, b := range ballots {
		ev, ok := byLabel[b.VoterLabel]
		if !ok {
			t.Errorf("missing event for voter %s", b.VoterLabel)
			continue
		}
		if ev.Round != 0 {
			t.Errorf("voter %s: Round = %d, want 0", b.VoterLabel, ev.Round)
		}
		if ev.VotedFor != "A" {
			t.Errorf("voter %s: VotedFor = %q, want A", b.VoterLabel, ev.VotedFor)
		}
		if ev.VotedForRealName != wantRealNameForA {
			t.Errorf("voter %s: VotedForRealName = %q, want %q",
				b.VoterLabel, ev.VotedForRealName, wantRealNameForA)
		}
		if string(ev.Body) != "VOTE: A\n" {
			t.Errorf("voter %s: Body = %q, want VOTE: A\\n", b.VoterLabel, string(ev.Body))
		}
		if ev.Resumed {
			t.Errorf("voter %s: Resumed = true on fresh run", b.VoterLabel)
		}
	}
}

// TestReporter_NilTolerated confirms that a nil RoundConfig.Reporter or
// BallotConfig.Reporter is normalized to NopReporter at fan-out entry, so
// runExpertR1/R2/runOneBallot never see a nil and the helpers don't need
// nil checks. Catches a refactor that drops the centralized normalization.
func TestReporter_NilTolerated(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "ok", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "5566778899aabbcc", exec)
	rcfg := RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
		// Reporter intentionally nil.
	}
	if _, err := RunRound1(context.Background(), rcfg, "q?"); err != nil {
		t.Fatalf("RunRound1 with nil Reporter: %v", err)
	}

	bexec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "VOTE: A\n", nil
		},
	}
	bcfg, _ := setupBallotTest(t, "ffeeddccbbaa9988", bexec)
	bcfg.Reporter = nil
	if _, err := RunBallot(context.Background(), bcfg, "q?", "agg"); err != nil {
		t.Fatalf("RunBallot with nil Reporter: %v", err)
	}

	// SelectOutput nil-Reporter path: must not panic when --verbose is off.
	r2 := []RoundOutput{{Label: "A", Name: "expert_a", Participation: "ok", Body: "body\n"}}
	if _, err := SelectOutput(s, TallyResult{Votes: map[string]int{"A": 1}, Winner: "A"}, r2, nil); err != nil {
		t.Fatalf("SelectOutput with nil Reporter: %v", err)
	}
}

// TestReporter_SelectOutput_FiresExtractionOnUniqueWinner pins that
// SelectOutput emits exactly one Kind == "extraction" event on the
// unique-winner path, with the winner's label + real name and the
// JSON-tail extraction status. The event is what cmd/council renders
// to the live verbose stream (ADR-0014 Task 5).
func TestReporter_SelectOutput_FiresExtractionOnUniqueWinner(t *testing.T) {
	s, _, _ := setupRoundTest(t, "abcd1234abcd1234", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	winnerBody := "prose body.\n\n```json\n{\"answer\": \"Clean.\"}\n```\n"
	r2 := []RoundOutput{
		{Label: "A", Name: "expert_a", Participation: "ok", Body: "A\n"},
		{Label: "B", Name: "expert_b", Participation: "ok", Body: winnerBody},
	}
	result := TallyResult{Votes: map[string]int{"A": 0, "B": 2}, Winner: "B"}
	rep := &recordingReporter{}
	if _, err := SelectOutput(s, result, r2, rep); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}

	events := rep.snapshot()
	if got, want := len(events), 1; got != want {
		t.Fatalf("len(events) = %d, want %d", got, want)
	}
	ev := events[0]
	if ev.Kind != "extraction" {
		t.Errorf("Kind = %q, want extraction", ev.Kind)
	}
	if ev.Label != "B" {
		t.Errorf("Label = %q, want B", ev.Label)
	}
	if ev.RealName != "expert_b" {
		t.Errorf("RealName = %q, want expert_b", ev.RealName)
	}
	if ev.ExtractStatus != ExtractOK {
		t.Errorf("ExtractStatus = %v, want ExtractOK", ev.ExtractStatus)
	}
	if string(ev.Body) != "Clean.\n" {
		t.Errorf("Body = %q, want \"Clean.\\n\" (extracted answer)", string(ev.Body))
	}
}

// TestReporter_SelectOutput_FiresExtractionOnFallback pins the parallel
// contract on the fail-closed path: the event still fires, with Body
// carrying the raw R2 bytes (what landed in output.md) and ExtractStatus
// reflecting the fallback reason.
func TestReporter_SelectOutput_FiresExtractionOnFallback(t *testing.T) {
	s, _, _ := setupRoundTest(t, "1234abcd1234abcd", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	rawBody := "raw R2 with no JSON tail at all.\n"
	r2 := []RoundOutput{
		{Label: "A", Name: "expert_a", Participation: "ok", Body: rawBody},
	}
	result := TallyResult{Votes: map[string]int{"A": 1}, Winner: "A"}
	rep := &recordingReporter{}
	if _, err := SelectOutput(s, result, r2, rep); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}

	events := rep.snapshot()
	if got, want := len(events), 1; got != want {
		t.Fatalf("len(events) = %d, want %d", got, want)
	}
	ev := events[0]
	if ev.Kind != "extraction" {
		t.Errorf("Kind = %q, want extraction", ev.Kind)
	}
	if ev.ExtractStatus != ExtractNoJSONBlock {
		t.Errorf("ExtractStatus = %v, want ExtractNoJSONBlock", ev.ExtractStatus)
	}
	if string(ev.Body) != rawBody {
		t.Errorf("Body = %q, want raw R2 body verbatim", string(ev.Body))
	}
}

// TestReporter_SelectOutput_NoExtractionEventOnTie pins that ties never
// emit an extraction event — extraction is unique-winner-only, so a tie
// stage produces zero extraction events.
func TestReporter_SelectOutput_NoExtractionEventOnTie(t *testing.T) {
	s, _, _ := setupRoundTest(t, "feedfacefeedface", &testExec{
		name: testExecName,
		fn:   func(ctx context.Context, req executor.Request, _ int) (string, error) { return "", nil },
	})
	r2 := []RoundOutput{
		{Label: "A", Name: "expert_a", Participation: "ok", Body: "A\n"},
		{Label: "B", Name: "expert_b", Participation: "ok", Body: "B\n"},
	}
	result := TallyResult{
		Votes:          map[string]int{"A": 1, "B": 1},
		TiedCandidates: []string{"A", "B"},
	}
	rep := &recordingReporter{}
	if _, err := SelectOutput(s, result, r2, rep); err != nil {
		t.Fatalf("SelectOutput: %v", err)
	}

	for _, ev := range rep.snapshot() {
		if ev.Kind == "extraction" {
			t.Errorf("unexpected extraction event on tie: %+v", ev)
		}
	}
}
