package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Verdict is the canonical machine-readable run index written to
// verdict.json at session end. Field order, JSON tags, and types gate the
// v2 schema contract in docs/design/v2.md §6 (fitness functions F3/F7/F8/F12).
// The bytes-exact snapshot test in verdict_test.go locks this shape.
type Verdict struct {
	Version         int               `json:"version"`
	SessionID       string            `json:"session_id"`
	SessionPath     string            `json:"session_path"`
	Profile         string            `json:"profile"`
	Question        string            `json:"question"`
	Answer          string            `json:"answer"`
	Status          string            `json:"status"`
	Anonymization   map[string]string `json:"anonymization,omitempty"`
	Rounds          []Round           `json:"rounds"`
	Experts         []ExpertSummary   `json:"experts"`
	Voting          *VerdictVoting    `json:"voting,omitempty"`
	StartedAt       string            `json:"started_at"`
	EndedAt         string            `json:"ended_at"`
	DurationSeconds float64           `json:"duration_seconds"`
}

// Round captures one debate round's per-expert outcomes. v2 emits one Round
// per debate round (blind R1, peer-aware R2). Experts slice is ordered
// alphabetically by Label so rerun of the same session yields identical
// bytes for identical expert outputs.
type Round struct {
	Experts []ExpertResult `json:"experts"`
}

// ExpertResult is the per-expert per-round record in verdict.json.rounds[].
// Label carries the anonymized single-letter token (ADR-0008); RealName
// is the profile's human-readable expert name; Participation is one of
// "ok" | "carried" | "failed" (see rounds.RoundOutput for the state model).
type ExpertResult struct {
	Label           string  `json:"label"`
	RealName        string  `json:"real_name"`
	Participation   string  `json:"participation"`
	Executor        string  `json:"executor"`
	Model           string  `json:"model"`
	ExitCode        int     `json:"exit_code"`
	Retries         int     `json:"retries"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// ExpertSummary is the top-level cross-round view of one expert.
// ParticipationByRound[i] is the Participation value for rounds[i].
// Length == len(Verdict.Rounds) — fitness F7 gates this invariant
// (docs/design/v2.md §8 F7).
type ExpertSummary struct {
	Label                string   `json:"label"`
	RealName             string   `json:"real_name"`
	ParticipationByRound []string `json:"participation_by_round"`
}

// VerdictVoting is the voting-stage summary surfaced in verdict.json. Exactly
// one of Winner (non-empty) or TiedCandidates (non-empty) is populated —
// fitness F12 gates that XOR invariant (docs/design/v2.md §8 F12). Votes
// carries one entry per active label (zero allowed for labels that
// received no ballots).
type VerdictVoting struct {
	Votes          map[string]int  `json:"votes"`
	Winner         string          `json:"winner,omitempty"`
	TiedCandidates []string        `json:"tied_candidates,omitempty"`
	Ballots        []VerdictBallot `json:"ballots"`
}

// VerdictBallot is one voter's entry in the voting summary. VotedFor is ""
// when the ballot was discarded (subprocess failure, forgery, malformed
// output, or vote for an inactive label).
type VerdictBallot struct {
	VoterLabel string `json:"voter_label"`
	VotedFor   string `json:"voted_for"`
}

// writeSyncCloser is the subset of *os.File the verdict writer needs. Tests
// substitute a spy implementation via openTempFile to assert call ordering
// (Write -> Sync -> Close -> rename).
type writeSyncCloser interface {
	io.Writer
	Sync() error
	Close() error
}

var (
	openTempFile = func(path string, flag int, perm os.FileMode) (writeSyncCloser, error) {
		return os.OpenFile(path, flag, perm)
	}
	renameFn = os.Rename
)

// WriteVerdict serializes v as indented JSON and writes it atomically to
// <session>/verdict.json: write to verdict.json.tmp with O_EXCL, fsync, close,
// then rename. Concurrent readers see either nothing, the previous file, or
// the new file — never a partial write.
//
// The O_EXCL on the .tmp file guarantees a single writer per session: if a
// stale .tmp exists (e.g. from a crashed prior run), this returns an error
// containing os.ErrExist and the operator must clear the stale file. We
// prefer surfacing that over silently overwriting and masking a concurrency
// bug.
func (s *Session) WriteVerdict(v *Verdict) error {
	buf, err := marshalVerdict(v)
	if err != nil {
		return fmt.Errorf("marshal verdict: %w", err)
	}
	tmpPath := filepath.Join(s.Path, "verdict.json.tmp")
	finalPath := filepath.Join(s.Path, "verdict.json")
	f, err := openTempFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpPath, err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := renameFn(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

// marshalVerdict serializes v with two-space indent and HTML escaping
// disabled. HTML escaping would mangle '<', '>', '&' inside question/answer
// bodies — verdict.json is not consumed by browsers and we want the bytes to
// match what the user wrote.
func marshalVerdict(v *Verdict) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
