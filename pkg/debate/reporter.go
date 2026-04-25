// Package debate's Reporter is the per-stage event hook that lets cmd/council
// stream the live debate to stderr as each expert/voter subprocess finishes.
//
// The reporter is called once per stage completion — round-1 expert, round-2
// expert, ballot — from the same goroutine that finished the work. Multiple
// stages run in parallel (rounds fan out N experts; voting fans out N voters),
// so implementations MUST be safe for concurrent calls. The default
// stderrReporter in cmd/council guards its writer with a sync.Mutex.
//
// debate code passes the body bytes that were already loaded for the
// forgery scan, so the reporter adds zero new I/O. NopReporter is the
// default when --verbose is off; debate calls it the same way and the
// no-op is inlined away by the compiler.
package debate

import (
	"time"

	"github.com/fitz123/council/pkg/runner"
)

// Reporter receives one StageEvent per completed stage. The interface is
// deliberately minimal — a single method — so adding a new stage type
// (e.g. "round-3-expert" if v4 ever adds a third round) is additive on the
// Kind discriminator rather than a breaking interface change.
type Reporter interface {
	OnStageDone(StageEvent)
}

// StageEvent is the per-stage payload. Kind discriminates the two event
// kinds ("round-expert" and "ballot"); within "round-expert", Round
// distinguishes the round-1 and round-2 shapes. Consumers switch on Kind
// and read only the fields relevant to that shape.
//
// Body is the raw subprocess stdout (output.md or votes/<label>.txt) the
// debate package already loaded for the forgery scan. It carries through
// untrusted LLM bytes — consumers should pipe it through stripControlBytes
// (or equivalent) before writing to a terminal.
type StageEvent struct {
	// Kind is "round-expert" or "ballot".
	Kind string

	// Round is 1 or 2 for round-expert events; 0 for ballot events.
	Round int

	// Label is the anonymized single-letter token (A, B, C…).
	Label string

	// RealName is the profile-level human-readable expert name.
	RealName string

	// Body is the subprocess stdout. Empty for failed stages where no
	// usable output landed; carries the carry-forward R1 bytes for
	// Participation == "carried".
	Body []byte

	// Participation applies to round-expert events: "ok" | "carried" | "failed".
	// Empty for ballot events (use VotedFor / LimitErr instead).
	Participation string

	// VotedFor applies to ballot events: the chosen label, or "" when the
	// ballot was discarded (subprocess fail, forgery, malformed, or vote
	// for an inactive label).
	VotedFor string

	// RejectedReason discriminates ballot-discard paths (one of the
	// debate.BallotRejected* constants). Empty for round-expert events
	// and for successful ballots. Lets the renderer distinguish the
	// failure modes that all otherwise show as "no vote".
	RejectedReason string

	// LimitErr is non-nil when the stage failed because of a vendor rate
	// limit (ADR-0013). For round-expert + Participation == "failed" or
	// for ballot + VotedFor == "" with LimitErr set, the renderer can
	// distinguish "rate-limited" from "malformed".
	LimitErr *runner.LimitError

	// Duration is the wall-clock time of the subprocess. Zero for
	// stages that resumed from a .done marker without re-spawning.
	Duration time.Duration

	// Retries is the number of fail-retry attempts before this final
	// outcome. Zero on the happy path; > 0 if MaxRetries kicked in.
	Retries int

	// Resumed is true when the stage was short-circuited by a pre-existing
	// .done marker (D14 resume path) rather than re-spawned. Body is still
	// populated (read from disk); Duration is zero.
	Resumed bool
}

// NopReporter is the default when --verbose is off. Debate code can call
// OnStageDone unconditionally without a nil check; the no-op is trivially
// inlinable by the Go compiler.
type NopReporter struct{}

// OnStageDone for NopReporter is a no-op. The receiver is a value (not a
// pointer) so callers can pass NopReporter{} as the field value directly.
func (NopReporter) OnStageDone(StageEvent) {}

// reportRoundExpert is the deferred fire-point used by runExpertR1 /
// runExpertR2. Taking a pointer to the result struct lets the defer
// closure see the FINAL state set by the body of those functions, not the
// zero state at defer-registration time. RunRound1 / RunRound2 normalize a
// nil RoundConfig.Reporter to NopReporter{} before fanning out, so this
// callee can assume rep is always non-nil.
func reportRoundExpert(rep Reporter, round int, ex LabeledExpert, r *RoundOutput, resumed bool) {
	rep.OnStageDone(StageEvent{
		Kind:          "round-expert",
		Round:         round,
		Label:         ex.Label,
		RealName:      ex.Role.Name,
		Body:          []byte(r.Body),
		Participation: r.Participation,
		LimitErr:      r.LimitErr,
		Duration:      time.Duration(r.DurationSeconds * float64(time.Second)),
		Retries:       r.Retries,
		Resumed:       resumed,
	})
}

// reportBallot is the deferred fire-point used by runOneBallot. Same
// pointer-to-result pattern as reportRoundExpert so the defer reflects the
// final outcome (success, discarded, or rate-limited). RunBallot normalizes
// a nil BallotConfig.Reporter to NopReporter{} before fanning out, so this
// callee can assume rep is always non-nil.
func reportBallot(rep Reporter, ex LabeledExpert, b *Ballot, body []byte, resumed bool) {
	rep.OnStageDone(StageEvent{
		Kind:           "ballot",
		Round:          0,
		Label:          ex.Label,
		RealName:       ex.Role.Name,
		Body:           body,
		VotedFor:       b.VotedFor,
		RejectedReason: b.RejectedReason,
		LimitErr:       b.LimitErr,
		Duration:       time.Duration(b.DurationSeconds * float64(time.Second)),
		Retries:        b.Retries,
		Resumed:        resumed,
	})
}
