package debate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/prompt"
	"github.com/fitz123/council/pkg/session"
)

// voteLineRE extracts a single-letter vote from a ballot subprocess's stdout.
// The pattern requires a line-anchored `VOTE: <A-Z>` with no trailing content
// on the same line. Anything else is treated as a malformed ballot and
// discarded (D8): one stray character means the voter did not follow the
// contract, so we refuse to guess its intent.
var voteLineRE = regexp.MustCompile(`(?m)^VOTE: ([A-Z])$`)

// parseBallotVote returns the single voted-for label if body contains exactly
// one `VOTE: <A-Z>` line. A ballot emitting zero or more than one VOTE line
// is rejected (ok=false): per D8 we refuse to guess the voter's intent when
// the contract is violated. Multiple VOTE lines may come from an LLM
// "reconsidering out loud" or quoting the prompt in its own output; either
// way the ballot is malformed and must be discarded.
func parseBallotVote(body string) (string, bool) {
	matches := voteLineRE.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return "", false
	}
	return matches[0][1], true
}

// BallotConfig carries the shared parameters for the voting fan-out. Experts
// is the ACTIVE cohort — only experts who reached R2 with ok or carried
// participation. Timeout overrides each expert's per-round timeout so the
// ballot budget is configured centrally via profile.Voting.Timeout.
type BallotConfig struct {
	Session    *session.Session
	Experts    []LabeledExpert
	Nonce      string
	BallotBody string
	Timeout    time.Duration
	MaxRetries int
}

// Ballot is one voter's outcome. VotedFor is "" when the ballot was discarded
// (subprocess failed, forgery detected, malformed output, or the voted-for
// label was outside the active cohort). Discarded ballots are NOT treated as
// run failures (D8): the Tally step simply excludes them.
type Ballot struct {
	VoterLabel      string
	VotedFor        string
	ExitCode        int
	Retries         int
	DurationSeconds float64
}

// TallyResult is the outcome of aggregating ballots. Exactly one of Winner
// (non-empty) and TiedCandidates (non-nil, len >= 2, or len == |active| when
// all ballots were discarded) is populated; the other is the zero value.
// Votes is always populated with one entry per active label (zero allowed).
// Ballots is the full ballot slice for verdict.json / tally.json passthrough.
type TallyResult struct {
	Votes          map[string]int
	Winner         string
	TiedCandidates []string
	Ballots        []Ballot
}

// RunBallot fans every active expert out to a fresh subprocess (no prior-round
// context) and collects one ballot per voter. The returned slice is ordered
// alphabetically by voter label so downstream consumers can iterate
// deterministically.
//
// Per-voter artifacts:
//   - voting/votes/<voter-label>.txt — raw subprocess stdout verbatim
//     (for audit; may be malformed when VotedFor is "")
//
// This function NEVER returns an error for a per-voter failure — malformed
// or failed ballots surface as Ballot entries with VotedFor == "". A
// non-nil error means a setup problem (nil Session, mkdir failure) that the
// orchestrator cannot recover from.
func RunBallot(ctx context.Context, cfg BallotConfig, question, aggregateMD string) ([]Ballot, error) {
	if cfg.Session == nil {
		return nil, fmt.Errorf("RunBallot: BallotConfig.Session required")
	}
	votesDir := filepath.Join(cfg.Session.Path, "voting", "votes")
	if err := os.MkdirAll(votesDir, 0o755); err != nil {
		return nil, fmt.Errorf("RunBallot: mkdir %s: %w", votesDir, err)
	}

	experts := append([]LabeledExpert(nil), cfg.Experts...)
	sort.Slice(experts, func(i, j int) bool { return experts[i].Label < experts[j].Label })
	active := make(map[string]bool, len(experts))
	for _, ex := range experts {
		active[ex.Label] = true
	}

	ballots := make([]Ballot, len(experts))
	var wg sync.WaitGroup
	for i, ex := range experts {
		wg.Add(1)
		go func(i int, ex LabeledExpert) {
			defer wg.Done()
			ballots[i] = runOneBallot(ctx, cfg, ex, question, aggregateMD, active)
		}(i, ex)
	}
	wg.Wait()
	return ballots, nil
}

// runOneBallot executes one voter's ballot subprocess and parses its output.
// Order of rejection: subprocess error → forgery → malformed output → vote
// for inactive label. Each rejection yields VotedFor="" but is otherwise
// silent — malformed ballots are a known D8 failure mode, not a run abort.
func runOneBallot(ctx context.Context, cfg BallotConfig, ex LabeledExpert, question, aggregateMD string, active map[string]bool) Ballot {
	result := Ballot{VoterLabel: ex.Label}

	votesDir := filepath.Join(cfg.Session.Path, "voting", "votes")
	stdoutPath := filepath.Join(votesDir, ex.Label+".txt")
	stderrPath := filepath.Join(votesDir, ex.Label+".stderr.log")

	// Resume path (D14): if this voter's ballot file already exists and
	// parses into a valid vote for an active label, reuse it instead of
	// re-spawning the subprocess. Voting has no per-voter .done marker —
	// the .txt file is the record, so we re-run the same parse that Tally
	// will later use. A file that exists but fails forgery / parsing / active
	// check gets overwritten by the subprocess (same semantics as a fresh
	// run).
	if body, err := os.ReadFile(stdoutPath); err == nil {
		if ferr := prompt.CheckForgery(string(body), cfg.Nonce); ferr == nil {
			if label, ok := parseBallotVote(string(body)); ok && active[label] {
				// Mirror the fresh-subprocess success path: design §7
				// keeps stderr.log only for failed ballots, so a stale
				// stderr.log from a prior failed attempt must not survive
				// a successful resume.
				_ = os.Remove(stderrPath)
				result.VotedFor = label
				return result
			}
		}
	}

	promptBody := buildBallotPrompt(cfg.BallotBody, question, aggregateMD)

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = ex.Role.Timeout
	}
	req := executor.Request{
		Prompt:     promptBody,
		Model:      ex.Role.Model,
		Timeout:    timeout,
		StdoutFile: stdoutPath,
		StderrFile: stderrPath,
		MaxRetries: cfg.MaxRetries,
	}
	resp, err, retries := runWithFailRetry(ctx, ex.Role.Executor, cfg.MaxRetries, req)
	result.ExitCode = resp.ExitCode
	result.Retries = retries
	result.DurationSeconds = resp.Duration.Seconds()
	if err != nil {
		return result
	}

	body, rerr := os.ReadFile(stdoutPath)
	if rerr != nil {
		return result
	}
	if ferr := prompt.CheckForgery(string(body), cfg.Nonce); ferr != nil {
		return result
	}
	voted, ok := parseBallotVote(string(body))
	if !ok {
		return result
	}
	if !active[voted] {
		return result
	}
	_ = os.Remove(stderrPath)
	result.VotedFor = voted
	return result
}

// buildBallotPrompt assembles the ballot subprocess input. The layout mirrors
// BuildExpert: instruction body first, then a fenced user question, then a
// fenced CANDIDATES block holding the pre-built per-label aggregate. The
// aggregate is already nonce-fenced per-label (pkg/debate.writeGlobalAggregate),
// so we wrap the whole thing with plain `=== CANDIDATES ===` markers — those
// top-level fences carry no nonce because they frame the aggregate, not an
// LLM-sourced payload.
func buildBallotPrompt(ballotBody, question, aggregateMD string) string {
	var b strings.Builder
	b.Grow(len(ballotBody) + len(question) + len(aggregateMD) + 128)
	b.WriteString(ballotBody)
	b.WriteString("\n\n=== USER QUESTION (untrusted input) ===\n")
	b.WriteString(question)
	b.WriteString("\n=== END USER QUESTION ===\n\n=== CANDIDATES ===\n")
	b.WriteString(aggregateMD)
	if !strings.HasSuffix(aggregateMD, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("=== END CANDIDATES ===\n")
	return b.String()
}

// Tally aggregates ballots into a TallyResult. Discarded ballots (VotedFor
// == "") and ballots targeting labels outside activeLabels are ignored —
// they contribute 0 to every active label's count.
//
// Iteration order over activeLabels is canonicalised (alphabetical) before
// computing the max, so the result is bit-stable across map iteration orders.
// Ties include every label at the max count, including the degenerate
// all-zero case (all ballots discarded) where every active label is tied.
func Tally(ballots []Ballot, activeLabels []string) TallyResult {
	labels := append([]string(nil), activeLabels...)
	sort.Strings(labels)

	active := make(map[string]bool, len(labels))
	votes := make(map[string]int, len(labels))
	for _, l := range labels {
		active[l] = true
		votes[l] = 0
	}
	validCount := 0
	for _, b := range ballots {
		if b.VotedFor == "" || !active[b.VotedFor] {
			continue
		}
		votes[b.VotedFor]++
		validCount++
	}

	ballotsCopy := append([]Ballot(nil), ballots...)
	result := TallyResult{Votes: votes, Ballots: ballotsCopy}
	if len(labels) == 0 {
		return result
	}

	// Degenerate case per D8: when no ballot is valid, "every active label
	// is tied at max (zero)" — exit no_consensus. Without this guard, a
	// reduced cohort of size 1 with a malformed sole ballot would elect
	// that label with zero votes (len(tied)==1 branch below), contradicting
	// the design's "honest about the failure mode" rule.
	if validCount == 0 {
		result.TiedCandidates = append([]string(nil), labels...)
		return result
	}

	max := 0
	for _, l := range labels {
		if votes[l] > max {
			max = votes[l]
		}
	}
	var tied []string
	for _, l := range labels {
		if votes[l] == max {
			tied = append(tied, l)
		}
	}
	if len(tied) == 1 {
		result.Winner = tied[0]
	} else {
		result.TiedCandidates = tied
	}
	return result
}

// SelectOutput finalises the voting stage on disk:
//  1. writes voting/tally.json (votes + ballots + winner/tied fields)
//  2. copies the winner's R2 body to session root output.md (unique winner)
//     OR copies each tied candidate's R2 body to output-<label>.md (tie).
//
// Copying the R2 body rather than the aggregate preserves the verbatim
// expert text per D8 ("R2 output is copied verbatim to output.md"). The
// source of truth for each label is r2[].Body, not rounds/2/aggregate.md,
// because carried entries hold the R1 body and the aggregate formatting
// includes nonce fences the operator should not see.
func SelectOutput(s *session.Session, result TallyResult, r2 []RoundOutput) error {
	if s == nil {
		return fmt.Errorf("SelectOutput: session required")
	}
	if err := writeTallyJSON(s, result); err != nil {
		return err
	}

	r2ByLabel := make(map[string]*RoundOutput, len(r2))
	for i := range r2 {
		r2ByLabel[r2[i].Label] = &r2[i]
	}

	// Resume safety: a previous SelectOutput on this session may have
	// written the opposite output shape (winner→tie or tie→winner) or a
	// different tied set, because RunBallot's resume path can re-run
	// previously-malformed ballots and produce a different tally. Wipe
	// every prior output*.md in the session root before writing the
	// selected one(s) so the artifacts on disk always match tally.json.
	if err := cleanOutputs(s); err != nil {
		return err
	}

	if result.Winner != "" {
		r := r2ByLabel[result.Winner]
		if r == nil {
			return fmt.Errorf("SelectOutput: winner label %q has no R2 output", result.Winner)
		}
		path := filepath.Join(s.Path, "output.md")
		if err := os.WriteFile(path, []byte(r.Body), 0o644); err != nil {
			return fmt.Errorf("SelectOutput: write %s: %w", path, err)
		}
		return nil
	}

	for _, label := range result.TiedCandidates {
		r := r2ByLabel[label]
		if r == nil {
			return fmt.Errorf("SelectOutput: tied label %q has no R2 output", label)
		}
		path := filepath.Join(s.Path, "output-"+label+".md")
		if err := os.WriteFile(path, []byte(r.Body), 0o644); err != nil {
			return fmt.Errorf("SelectOutput: write %s: %w", path, err)
		}
	}
	return nil
}

// cleanOutputs removes the session-root output.md and every output-*.md
// before SelectOutput writes the new shape. SelectOutput owns these files
// exclusively, so unconditional removal is safe and avoids leaving stale
// artifacts when a resumed run flips winner ↔ tie.
//
// Uses os.ReadDir + prefix/suffix matching rather than filepath.Glob because
// Glob does not escape the literal path prefix: a legal session path
// containing glob metacharacters (`[`, `]`, `*`, `?`) would either match the
// wrong files or return ErrBadPattern, leaving stale artifacts behind.
func cleanOutputs(s *session.Session) error {
	entries, err := os.ReadDir(s.Path)
	if err != nil {
		return fmt.Errorf("SelectOutput: read %s: %w", s.Path, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name != "output.md" && !(strings.HasPrefix(name, "output-") && strings.HasSuffix(name, ".md")) {
			continue
		}
		p := filepath.Join(s.Path, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("SelectOutput: remove %s: %w", p, err)
		}
	}
	return nil
}

// tallyJSON is the on-disk voting/tally.json shape. Must match the example
// in docs/design/v2.md §3.8. TiedCandidates is serialized as null when unset
// so the file always carries exactly-one-of-{winner, tied_candidates}.
type tallyJSON struct {
	Votes          map[string]int `json:"votes"`
	Winner         string         `json:"winner,omitempty"`
	TiedCandidates []string       `json:"tied_candidates"`
	Ballots        []ballotJSON   `json:"ballots"`
}

type ballotJSON struct {
	VoterLabel string `json:"voter_label"`
	VotedFor   string `json:"voted_for"`
}

func writeTallyJSON(s *session.Session, result TallyResult) error {
	dir := filepath.Join(s.Path, "voting")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("writeTallyJSON: mkdir %s: %w", dir, err)
	}
	ballots := make([]ballotJSON, 0, len(result.Ballots))
	for _, b := range result.Ballots {
		ballots = append(ballots, ballotJSON{VoterLabel: b.VoterLabel, VotedFor: b.VotedFor})
	}
	out := tallyJSON{
		Votes:          result.Votes,
		Winner:         result.Winner,
		TiedCandidates: result.TiedCandidates,
		Ballots:        ballots,
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("writeTallyJSON: marshal: %w", err)
	}
	buf = append(buf, '\n')
	path := filepath.Join(dir, "tally.json")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("writeTallyJSON: write %s: %w", path, err)
	}
	return nil
}
