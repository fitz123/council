// Package orchestrator wires the v2 debate pipeline end-to-end: session
// setup (nonce, anonymization, question sanity scan), round 1 blind fan-out,
// round 2 peer-aware fan-out, ballot stage, tally, output selection, and
// atomic verdict emission. The actual subprocess work lives in pkg/debate;
// this package owns the outer control flow — injection gating, quorum
// escalation, interrupt handling, verdict assembly.
//
// Signal handling: the caller (cmd/council) owns SIGINT/SIGTERM and cancels
// the root context. Run respects ctx.Done() between every stage, always
// writes verdict.json before returning (status "interrupted"), and omits the
// root-level .done marker on interrupted runs so `council resume` (D14) can
// pick the session up later.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/debate"
	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/prompt"
	"github.com/fitz123/council/pkg/session"
)

// Sentinel errors mapped to exit codes by cmd/council. Verdict.json is
// written to disk BEFORE any of these is returned, so the caller can exit
// immediately on receipt.
var (
	// ErrInterrupted: root context cancelled mid-run. Verdict has
	// status="interrupted"; no root .done marker is written so resume
	// (D14) can pick up.
	ErrInterrupted = errors.New("orchestrator: interrupted")

	// ErrNoConsensus: voting ended in a tie (any N>=2 tied candidates).
	// One output-<label>.md file per tied candidate lands in the session
	// root. Maps to exit 2.
	ErrNoConsensus = errors.New("orchestrator: no consensus")

	// ErrInjectionInQuestion: operator-supplied question contained a
	// fence-shaped line. No subprocesses are spawned. Verdict has
	// status="injection_suspected_in_question". Maps to exit 1.
	ErrInjectionInQuestion = errors.New("orchestrator: injection suspected in question")
)

// Validate checks that every executor name referenced by the profile is
// registered. v2 retired the judge role (D15); only expert executors are
// exercised here.
func Validate(p *config.Profile) error {
	var missing []string
	for _, e := range p.Experts {
		if _, err := executor.Get(e.Executor); err != nil {
			missing = append(missing, fmt.Sprintf("expert %q uses unknown executor %q", e.Name, e.Executor))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("profile %q: %s", p.Name, strings.Join(missing, "; "))
	}
	return nil
}

// timestampLayout matches docs/design/v1.md §6 example
// ("2026-04-19T17:02:14Z") — RFC3339 with colon-separated time.
const timestampLayout = time.RFC3339

// Run drives one debate session end-to-end. It always writes verdict.json
// before returning: on ok, on quorum failure, on tie (no_consensus), on
// injection rejection, and on interruption. The returned Verdict is the
// same struct flushed to disk.
//
// The caller is expected to have already invoked session.Create (via
// cmd/council.createSession) to materialise the session folder with the
// nonce baked into profile.snapshot.yaml.
//
// reporter receives one OnStageDone call as each round-expert and ballot
// stage completes. Pass debate.NopReporter{} when no live streaming is
// wanted (i.e. when --verbose is off); the no-op call is inlinable.
func Run(ctx context.Context, profile *config.Profile, question string, sess *session.Session, reporter debate.Reporter) (*session.Verdict, error) {
	// On resume, an "interrupted" verdict.json from the previous attempt
	// carries the original run's started_at AND its rate_limits[]. Preserve
	// both: started_at so duration_seconds reflects total wall-clock from
	// session creation to final resolution; rate_limits so the audit trail
	// (F33/F35) survives a resume boundary — re-running a round on resume
	// short-circuits via .done/.failed/.carried markers without restoring
	// in-memory LimitErr, so collectRoundLimits would otherwise produce an
	// empty slice and the prior rate-limit entries would be lost. Parse
	// errors / absence fall back to time.Now() with no prior limits — the
	// common case is a fresh run with no prior verdict.
	startedAt := time.Now().UTC()
	var priorLimits []session.RateLimitEntry
	if prior, limits, ok := readInterruptedVerdictState(sess.Path); ok {
		startedAt = prior
		priorLimits = limits
	}
	v := &session.Verdict{
		Version:     2,
		SessionID:   sess.ID,
		SessionPath: verdictSessionPath(sess.ID),
		Profile:     profile.Name,
		Question:    question,
		StartedAt:   startedAt.Format(timestampLayout),
		RateLimits:  priorLimits,
	}

	// Stage 1: sanity-scan the operator question for fence-shaped lines.
	// A raw `=== EXPERT: ... ===` in the question would poison every
	// downstream prompt, so we reject at the boundary with a dedicated
	// status rather than letting the broken run complete.
	if err := prompt.ScanQuestionForInjection(question); err != nil {
		v.Status = "injection_suspected_in_question"
		return finalizeAndWrite(v, sess, startedAt, fmt.Errorf("%w: %v", ErrInjectionInQuestion, err))
	}

	// Stage 2: anonymization. The mapping is derived deterministically
	// from sess.ID, so resume re-derives the same labels without needing
	// a persisted copy.
	experts := make([]debate.Expert, len(profile.Experts))
	for i, e := range profile.Experts {
		experts[i] = debate.Expert{Name: e.Name}
	}
	mapping, err := debate.AssignLabels(sess.ID, experts)
	if err != nil {
		v.Status = "config_error"
		return finalizeAndWrite(v, sess, startedAt, err)
	}
	v.Anonymization = mapping

	labeled := buildLabeled(profile.Experts, mapping)
	rcfg := debate.RoundConfig{
		Session:      sess,
		Experts:      labeled,
		Quorum:       profile.Quorum,
		MaxRetries:   profile.MaxRetries,
		Nonce:        sess.Nonce,
		R2PromptBody: profile.Round2Prompt.Body,
		Reporter:     reporter,
	}

	// Stage 3: round 1 (blind fan-out).
	r1, err := debate.RunRound1(ctx, rcfg, question)
	v.Rounds = append(v.Rounds, buildRoundVerdict(r1, profile))
	v.RateLimits = append(v.RateLimits, collectRoundLimits(r1, 1)...)
	if ctx.Err() != nil {
		return finalizeInterrupted(v, sess, startedAt)
	}
	if err != nil {
		// ErrRateLimitQuorumFail is checked BEFORE the generic R1
		// sentinel: the two are disjoint (debate exposes them as distinct
		// errors so cmd/council can map only the rate-limit case to exit
		// 6), but ordering the rate-limit branch first keeps the intent
		// readable.
		if errors.Is(err, debate.ErrRateLimitQuorumFail) {
			v.Status = "rate_limit_quorum_failed"
			return finalizeAndWrite(v, sess, startedAt, err)
		}
		if errors.Is(err, debate.ErrQuorumFailedR1) {
			v.Status = "quorum_failed_round_1"
			return finalizeAndWrite(v, sess, startedAt, err)
		}
		v.Status = "error"
		return finalizeAndWrite(v, sess, startedAt, err)
	}

	// Stage 4: round 2 (peer-aware, with carry-forward on per-expert fail).
	r2, err := debate.RunRound2(ctx, rcfg, question, r1)
	v.Rounds = append(v.Rounds, buildRoundVerdict(r2, profile))
	v.RateLimits = append(v.RateLimits, collectRoundLimits(r2, 2)...)
	if ctx.Err() != nil {
		return finalizeInterrupted(v, sess, startedAt)
	}
	if err != nil {
		if errors.Is(err, debate.ErrRateLimitQuorumFail) {
			v.Status = "rate_limit_quorum_failed"
			return finalizeAndWrite(v, sess, startedAt, err)
		}
		if errors.Is(err, debate.ErrQuorumFailedR2) {
			v.Status = "quorum_failed_round_2"
			return finalizeAndWrite(v, sess, startedAt, err)
		}
		v.Status = "error"
		return finalizeAndWrite(v, sess, startedAt, err)
	}

	// Stage 5: ballot. Active cohort = R2 ok+carried.
	aggregatePath := filepath.Join(sess.Path, "rounds", "2", "aggregate.md")
	aggregateMD, rerr := os.ReadFile(aggregatePath)
	if rerr != nil {
		v.Status = "error"
		return finalizeAndWrite(v, sess, startedAt, fmt.Errorf("read r2 aggregate: %w", rerr))
	}
	active, activeLabels := activeCohort(r2, labeled)

	bcfg := debate.BallotConfig{
		Session:    sess,
		Experts:    active,
		Nonce:      sess.Nonce,
		BallotBody: profile.Voting.BallotPromptBody,
		Timeout:    profile.Voting.Timeout,
		MaxRetries: profile.MaxRetries,
		Reporter:   reporter,
	}
	ballots, err := debate.RunBallot(ctx, bcfg, question, string(aggregateMD))
	v.RateLimits = append(v.RateLimits, collectBallotLimits(ballots)...)
	if ctx.Err() != nil {
		return finalizeInterrupted(v, sess, startedAt)
	}
	if err != nil {
		v.Status = "error"
		return finalizeAndWrite(v, sess, startedAt, err)
	}

	tally := debate.Tally(ballots, activeLabels)
	if err := debate.SelectOutput(sess, tally, r2); err != nil {
		v.Status = "error"
		return finalizeAndWrite(v, sess, startedAt, err)
	}
	v.Voting = buildVotingVerdict(tally)

	// Unique winner: copy winner's R2 body into v.Answer so cmd/council
	// can print it to stdout.
	if tally.Winner != "" {
		for _, o := range r2 {
			if o.Label == tally.Winner {
				v.Answer = o.Body
				break
			}
		}
		v.Status = "ok"
		return finalizeAndWrite(v, sess, startedAt, nil)
	}

	// Tie: status no_consensus, ErrNoConsensus so cmd/council exits 2.
	v.Status = "no_consensus"
	return finalizeAndWrite(v, sess, startedAt, ErrNoConsensus)
}

// finalizeAndWrite stamps the end-time, writes verdict.json, and drops the
// root-level .done finality marker for every non-interrupted terminal path
// (D14: the marker tells `council resume` that the session has reached a
// final state). Only the interrupt path skips .done so resume can pick up
// where SIGINT left off.
func finalizeAndWrite(v *session.Verdict, sess *session.Session, startedAt time.Time, runErr error) (*session.Verdict, error) {
	finalize(v, startedAt)
	if werr := sess.WriteVerdict(v); werr != nil {
		// errors.Join preserves both the I/O failure and the original
		// sentinel (e.g. ErrNoConsensus, ErrQuorumFailedR1) so
		// cmd/council's errors.Is switch still maps the exit code
		// correctly when verdict.json write fails.
		return v, errors.Join(fmt.Errorf("write verdict: %w", werr), runErr)
	}
	if derr := writeSessionDone(sess); derr != nil {
		return v, errors.Join(fmt.Errorf("write session .done: %w", derr), runErr)
	}
	return v, runErr
}

// finalizeInterrupted flushes verdict.json with status="interrupted" and
// returns ErrInterrupted. No root-level .done marker is written: `council
// resume` (D14) uses .done absence as the "this session can still progress"
// signal.
func finalizeInterrupted(v *session.Verdict, sess *session.Session, startedAt time.Time) (*session.Verdict, error) {
	v.Status = "interrupted"
	finalize(v, startedAt)
	if err := sess.WriteVerdict(v); err != nil {
		// errors.Join preserves ErrInterrupted alongside the I/O failure
		// so cmd/council's errors.Is switch still maps to exit 130 when
		// verdict.json write fails on the SIGINT path.
		return v, errors.Join(fmt.Errorf("write verdict: %w", err), ErrInterrupted)
	}
	return v, ErrInterrupted
}

// readInterruptedVerdictState recovers state from a prior interrupted
// verdict.json that the resume path needs to thread into the new run:
// the original started_at (for total wall-clock duration) and the prior
// rate_limits[] (so the audit trail survives — round runners short-
// circuit completed stages without restoring in-memory LimitErr, so
// collectRoundLimits would drop these entries). Only verdicts with
// status="interrupted" are honoured — any other status means the session
// is final (resume would refuse it) or the verdict is a prior resume's
// own output being overwritten. Missing / unparseable / non-interrupted
// verdicts return ok=false and the caller falls back to time.Now() with
// no prior limits, preserving the fresh-run behaviour.
func readInterruptedVerdictState(sessionPath string) (time.Time, []session.RateLimitEntry, bool) {
	data, err := os.ReadFile(filepath.Join(sessionPath, "verdict.json"))
	if err != nil {
		return time.Time{}, nil, false
	}
	var v struct {
		Status     string                   `json:"status"`
		StartedAt  string                   `json:"started_at"`
		RateLimits []session.RateLimitEntry `json:"rate_limits"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return time.Time{}, nil, false
	}
	if v.Status != "interrupted" || v.StartedAt == "" {
		return time.Time{}, nil, false
	}
	t, err := time.Parse(timestampLayout, v.StartedAt)
	if err != nil {
		return time.Time{}, nil, false
	}
	return t.UTC(), v.RateLimits, true
}

// finalize sets the end-timestamp and duration fields on v using the clock
// captured at Run start, and derives the top-level experts[] summary
// (F7/F8 gate) from v.Rounds. Called from every terminal path so
// verdict.json always carries meaningful started_at/ended_at/
// duration_seconds values plus an experts array that matches rounds.
func finalize(v *session.Verdict, startedAt time.Time) {
	end := time.Now().UTC()
	v.EndedAt = end.Format(timestampLayout)
	v.DurationSeconds = end.Sub(startedAt).Seconds()
	v.Experts = buildExpertsSummary(v)
}

// writeSessionDone writes the root-level .done marker that D14 defines as
// the finality signal for `council resume`. Every non-interrupted terminal
// path emits it (winner, tie, quorum failure, injection, config error);
// only the interrupt path skips it so resume can still pick the session
// up. The resume predicate also checks verdict.status as a defense-in-depth
// signal against a missing/corrupted .done.
func writeSessionDone(sess *session.Session) error {
	donePath := filepath.Join(sess.Path, ".done")
	f, err := os.OpenFile(donePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", donePath, err)
	}
	return f.Close()
}

// buildLabeled pairs each profile expert with its anonymized label, sorted
// alphabetically by label so downstream iteration is deterministic.
func buildLabeled(roles []config.RoleConfig, mapping map[string]string) []debate.LabeledExpert {
	labeled := make([]debate.LabeledExpert, 0, len(roles))
	for i := range roles {
		label, ok := debate.LabelOf(roles[i].Name, mapping)
		if !ok {
			// AssignLabels covers every expert; this is unreachable.
			continue
		}
		labeled = append(labeled, debate.LabeledExpert{Label: label, Role: roles[i]})
	}
	sort.Slice(labeled, func(i, j int) bool { return labeled[i].Label < labeled[j].Label })
	return labeled
}

// buildRoundVerdict renders a slice of debate.RoundOutput into the
// verdict.json Round shape, using the profile to backfill each role's
// executor/model.
func buildRoundVerdict(outputs []debate.RoundOutput, p *config.Profile) session.Round {
	byName := make(map[string]config.RoleConfig, len(p.Experts))
	for _, e := range p.Experts {
		byName[e.Name] = e
	}
	experts := make([]session.ExpertResult, 0, len(outputs))
	for _, o := range outputs {
		role := byName[o.Name]
		experts = append(experts, session.ExpertResult{
			Label:           o.Label,
			RealName:        o.Name,
			Participation:   o.Participation,
			Executor:        role.Executor,
			Model:           role.Model,
			ExitCode:        o.ExitCode,
			Retries:         o.Retries,
			DurationSeconds: o.DurationSeconds,
		})
	}
	return session.Round{Experts: experts}
}

// buildExpertsSummary collapses v.Rounds into the top-level experts[]
// summary that fitness F7 gates: one entry per distinct label, with
// ParticipationByRound[i] mirroring Rounds[i].Experts[*].Participation for
// that label. Output is ordered alphabetically by label so verdict bytes
// are stable across runs with identical round data.
//
// Labels are drawn from v.Rounds (not v.Anonymization) so an abort before
// round 1 produces an empty summary rather than a labels-without-rounds
// inconsistency.
func buildExpertsSummary(v *session.Verdict) []session.ExpertSummary {
	type accum struct {
		realName string
		parts    []string
	}
	byLabel := make(map[string]*accum)
	order := make([]string, 0)
	for roundIdx, r := range v.Rounds {
		for _, e := range r.Experts {
			a, ok := byLabel[e.Label]
			if !ok {
				a = &accum{realName: e.RealName, parts: make([]string, len(v.Rounds))}
				byLabel[e.Label] = a
				order = append(order, e.Label)
			}
			a.parts[roundIdx] = e.Participation
		}
	}
	sort.Strings(order)
	out := make([]session.ExpertSummary, 0, len(order))
	for _, label := range order {
		a := byLabel[label]
		out = append(out, session.ExpertSummary{
			Label:                label,
			RealName:             a.realName,
			ParticipationByRound: a.parts,
		})
	}
	return out
}

// buildVotingVerdict translates a debate.TallyResult into the verdict.json
// Voting shape. Exactly one of Winner / TiedCandidates is populated; the
// other is the zero value (preserved from tally).
func buildVotingVerdict(t debate.TallyResult) *session.VerdictVoting {
	ballots := make([]session.VerdictBallot, 0, len(t.Ballots))
	for _, b := range t.Ballots {
		ballots = append(ballots, session.VerdictBallot{
			VoterLabel: b.VoterLabel,
			VotedFor:   b.VotedFor,
		})
	}
	return &session.VerdictVoting{
		Votes:          t.Votes,
		Winner:         t.Winner,
		TiedCandidates: t.TiedCandidates,
		Ballots:        ballots,
	}
}

// activeCohort filters the labeled expert slice to those whose R2
// participation was ok or carried (D3: carry-forward preserves voting
// participation). Returned labels are alphabetical for stable tally
// iteration.
func activeCohort(r2 []debate.RoundOutput, labeled []debate.LabeledExpert) ([]debate.LabeledExpert, []string) {
	byLabel := make(map[string]debate.LabeledExpert, len(labeled))
	for _, le := range labeled {
		byLabel[le.Label] = le
	}
	active := make([]debate.LabeledExpert, 0, len(r2))
	labels := make([]string, 0, len(r2))
	for _, o := range r2 {
		if o.Participation != "ok" && o.Participation != "carried" {
			continue
		}
		le, ok := byLabel[o.Label]
		if !ok {
			continue
		}
		active = append(active, le)
		labels = append(labels, o.Label)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Label < active[j].Label })
	sort.Strings(labels)
	return active, labels
}

func verdictSessionPath(id string) string {
	return "./" + filepath.ToSlash(filepath.Join(".council", "sessions", id))
}

// collectRoundLimits walks a slice of debate.RoundOutput and returns one
// session.RateLimitEntry per output whose LimitErr is non-nil. Round is the
// 1-based round number stamped on each entry so verdict.json can carry an
// audit trail across R1, R2, and the ballot stage. The slice is ordered by
// expert label (matching the input slice's existing alphabetical order from
// debate.RunRound1 / RunRound2) so verdict bytes stay stable across runs.
func collectRoundLimits(outputs []debate.RoundOutput, round int) []session.RateLimitEntry {
	out := make([]session.RateLimitEntry, 0)
	for _, o := range outputs {
		if o.LimitErr == nil {
			continue
		}
		out = append(out, session.RateLimitEntry{
			Executor: o.LimitErr.Tool,
			Pattern:  o.LimitErr.Pattern,
			HelpCmd:  o.LimitErr.HelpCmd,
			Round:    round,
			Expert:   o.Label,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectBallotLimits is the ballot-stage analogue of collectRoundLimits.
// Round 0 is reserved for the ballot stage so verdict.json readers can
// distinguish ballots from R1/R2 expert spawns when scanning rate_limits[].
func collectBallotLimits(ballots []debate.Ballot) []session.RateLimitEntry {
	out := make([]session.RateLimitEntry, 0)
	for _, b := range ballots {
		if b.LimitErr == nil {
			continue
		}
		out = append(out, session.RateLimitEntry{
			Executor: b.LimitErr.Tool,
			Pattern:  b.LimitErr.Pattern,
			HelpCmd:  b.LimitErr.HelpCmd,
			Round:    0,
			Expert:   b.VoterLabel,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
