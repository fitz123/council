package debate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/prompt"
	"github.com/fitz123/council/pkg/runner"
	"github.com/fitz123/council/pkg/session"
)

// defaultExpertTools is the hardcoded ADR-0010 D17 allow-list granted to
// every R1 and R2 expert spawn. The values are constants here rather than
// threaded through profile/config so there is exactly one source of truth
// for "what tools does an expert get" inside pkg/debate; ballots explicitly
// set AllowedTools=nil in vote.go so voting stays tools-off regardless of
// this default (F17).
//
// Callers MUST use expertAllowedTools() instead of reading the package
// slice directly: the Request type is a public executor contract and a
// future executor implementation (Codex, Gemini, ...) may normalize or
// dedupe req.AllowedTools in place. Without a per-request copy, that
// mutation would race across sibling expert goroutines and could
// permanently corrupt the shared default.
var defaultExpertTools = []string{"WebSearch", "WebFetch"}

// expertAllowedTools returns a fresh copy of defaultExpertTools. See the
// comment on defaultExpertTools for the concurrency rationale.
func expertAllowedTools() []string {
	return append([]string(nil), defaultExpertTools...)
}

// defaultPermissionMode is the companion to defaultExpertTools. The claude-code
// executor only emits `--permission-mode` when this field is non-empty, so the
// `bypassPermissions` value is what actually lets the allow-listed tools run
// without an interactive approval prompt inside the subprocess.
const defaultPermissionMode = "bypassPermissions"

// ErrQuorumFailedR1 is the sentinel returned by RunRound1 when the count of
// surviving experts is below the profile's quorum. The orchestrator maps this
// to verdict.status = "quorum_failed_round_1" and exit code 2. Outputs are
// still returned alongside the error so the caller can record per-expert
// failure details in verdict.json.
var ErrQuorumFailedR1 = errors.New("debate: round 1 quorum not met")

// ErrQuorumFailedR2 mirrors ErrQuorumFailedR1 for round 2. Carry-forward
// preserves R2 participation for experts whose R2 subprocess failed after
// retry, so reaching this sentinel requires enough experts to have failed
// round 1 (been dropped from the cohort) that the reduced round-2 cohort
// cannot meet quorum.
var ErrQuorumFailedR2 = errors.New("debate: round 2 quorum not met")

// ErrRateLimitQuorumFail is returned by RunRound1 / RunRound2 when survivors
// fall below quorum AND at least one of the failures was a *runner.LimitError
// (ADR-0013). This is intentionally distinct from ErrQuorumFailedR1 /
// ErrQuorumFailedR2: cmd/council maps it to exit code 6 with a per-CLI help
// footer, so errors.Is must NOT also match the round-specific sentinels.
//
// When quorum is met despite some experts hitting rate-limits, the round
// returns nil (the LimitErrors are still recorded on RoundOutput.LimitErr
// and surface in verdict.json's rate_limits[]).
var ErrRateLimitQuorumFail = errors.New("debate: quorum not met due to rate-limit failures")

// LabeledExpert pairs a profile role with its session-scoped anonymized
// label. The orchestrator builds this slice after calling AssignLabels; the
// round runners consume it directly so they never need the mapping itself.
type LabeledExpert struct {
	Label string
	Role  config.RoleConfig
}

// RoundConfig carries the parameters shared across a single round's fan-out.
// Session, Experts, and Nonce are mandatory; MaxRetries and Quorum are
// profile-derived numbers the runner uses for per-expert policy and the
// round-level quorum gate respectively. R2PromptBody is the profile-level
// peer-aware role prompt that replaces each expert's R1 PromptBody in round
// 2 (design §3.4). RunRound1 ignores it.
type RoundConfig struct {
	Session      *session.Session
	Experts      []LabeledExpert
	Quorum       int
	MaxRetries   int
	Nonce        string
	R2PromptBody string
}

// RoundOutput captures one expert's outcome for a single round. Body holds
// the raw subprocess stdout on success; on "failed" it is empty so the caller
// cannot accidentally propagate leaked bytes into a downstream prompt. On
// "carried" (R2 only) it holds the R1 body that was copied forward.
//
// LimitErr is non-nil when the expert subprocess failed because it returned
// a *runner.LimitError (ADR-0013); the orchestrator collects these into
// verdict.json's rate_limits[] and decides exit code 6 vs the regular
// quorum-fail path. LimitErr is independent of Participation: a rate-limited
// R2 expert with R1 success is still "carried" with LimitErr populated.
type RoundOutput struct {
	Label           string
	Name            string
	Participation   string // "ok" | "carried" | "failed"
	Body            string
	ExitCode        int
	Retries         int
	DurationSeconds float64
	LimitErr        *runner.LimitError
}

// RunRound1 fans experts out in blind parallel: no expert sees any other's
// output (R1 is the "independent" round per ADR-0008). Each expert writes
// prompt.md, output.md, and — on success only — .done into its own
// rounds/1/experts/<label>/ directory. The .done marker is the finality
// signal consumed by resume (D14); it lands only after the forgery check
// passes, so a subprocess that echoed the session nonce is never treated
// as complete.
//
// The returned slice contains ALL experts regardless of outcome, ordered
// alphabetically by label, so downstream callers can report on failures
// in verdict.json. On quorum failure the slice is still populated and
// ErrQuorumFailedR1 is returned.
func RunRound1(ctx context.Context, cfg RoundConfig, question string) ([]RoundOutput, error) {
	if cfg.Session == nil {
		return nil, fmt.Errorf("RunRound1: RoundConfig.Session required")
	}

	experts := append([]LabeledExpert(nil), cfg.Experts...)
	sort.Slice(experts, func(i, j int) bool { return experts[i].Label < experts[j].Label })

	outputs := make([]RoundOutput, len(experts))
	var wg sync.WaitGroup
	for i, ex := range experts {
		wg.Add(1)
		go func(i int, ex LabeledExpert) {
			defer wg.Done()
			outputs[i] = runExpertR1(ctx, cfg, ex, question)
		}(i, ex)
	}
	wg.Wait()

	survivors := 0
	for _, o := range outputs {
		if o.Participation == "ok" {
			survivors++
		}
	}
	if survivors < cfg.Quorum {
		// Rate-limit quorum-fail wins over the generic round-1 sentinel
		// when ANY failed expert returned a *runner.LimitError. The two
		// sentinels are intentionally disjoint (errors.Is matches one and
		// not the other) so cmd/council can map this case to exit 6 with
		// a per-CLI help footer instead of the standard exit 2.
		if hasLimitErr(outputs) {
			return outputs, fmt.Errorf("%w: %d survivors < quorum %d", ErrRateLimitQuorumFail, survivors, cfg.Quorum)
		}
		return outputs, fmt.Errorf("%w: %d survivors < quorum %d", ErrQuorumFailedR1, survivors, cfg.Quorum)
	}
	return outputs, nil
}

// hasLimitErr reports whether any RoundOutput in outputs was attributed to a
// rate-limit failure. Used by RunRound1 / RunRound2 to choose between the
// generic quorum-fail sentinel and ErrRateLimitQuorumFail when survivors fall
// below cfg.Quorum. A single rate-limited failure flips the round to the
// rate-limit sentinel — the orchestrator can still record the non-rate-limit
// failures in verdict.json (those have LimitErr=nil).
func hasLimitErr(outputs []RoundOutput) bool {
	for _, o := range outputs {
		if o.LimitErr != nil {
			return true
		}
	}
	return false
}

// runExpertR1 executes one expert's R1 lifecycle: mkdir stage dir, build +
// write prompt.md, invoke the executor with orchestrator-owned fail-retry,
// validate the output against forgery, and touch .done on success. The
// resulting RoundOutput records every observable field so the caller can
// feed it directly into verdict.json without a second read.
func runExpertR1(ctx context.Context, cfg RoundConfig, ex LabeledExpert, question string) RoundOutput {
	result := RoundOutput{
		Label:         ex.Label,
		Name:          ex.Role.Name,
		Participation: "failed",
	}

	dir := cfg.Session.RoundExpertDir(1, ex.Label)
	// Resume path (D14): if the stage .done marker already exists, trust the
	// prior run's finalize and short-circuit. We re-emit the output body
	// into memory so R2 aggregation still sees the right bytes; no
	// subprocess is spawned. This keeps `council resume` O(cost of missing
	// experts) rather than O(cost of full rerun).
	if body, ok := readCompletedStage(dir); ok {
		result.Participation = "ok"
		result.Body = body
		return result
	}
	// R1 failures are permanent per D3 ("expert is DROPPED from session").
	// A .failed marker left by a prior attempt freezes the drop across
	// resume — otherwise a previously-failed expert could be re-spawned,
	// succeed, and land in a cohort whose other R2 .done outputs were built
	// without it.
	if _, err := os.Stat(filepath.Join(dir, ".failed")); err == nil {
		return result
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return result
	}

	promptBody := prompt.BuildExpert(ex.Role.PromptBody, question, cfg.Nonce)
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(promptBody), 0o644); err != nil {
		return result
	}

	req := executor.Request{
		Prompt:         promptBody,
		Model:          ex.Role.Model,
		Timeout:        ex.Role.Timeout,
		StdoutFile:     filepath.Join(dir, "output.md"),
		StderrFile:     filepath.Join(dir, "stderr.log"),
		MaxRetries:     cfg.MaxRetries,
		AllowedTools:   expertAllowedTools(),
		PermissionMode: defaultPermissionMode,
	}
	resp, err, retries := runWithFailRetry(ctx, ex.Role.Executor, cfg.MaxRetries, req)
	result.ExitCode = resp.ExitCode
	result.Retries = retries
	result.DurationSeconds = resp.Duration.Seconds()

	if err != nil {
		var le *runner.LimitError
		if errors.As(err, &le) {
			result.LimitErr = le
		}
		// Retry budget spent: per D3 the drop is permanent, so record a
		// .failed tombstone. Skip on ctx cancellation — SIGINT mid-retry
		// is "come back later," not a definitive failure.
		if ctx.Err() == nil {
			_ = os.WriteFile(filepath.Join(dir, ".failed"), nil, 0o644)
		}
		return result
	}

	body, rerr := os.ReadFile(req.StdoutFile)
	if rerr != nil {
		// Post-subprocess local I/O failure on the orchestrator host.
		// Not a D3 after-retry expert failure — this is a FS problem.
		// Tombstone anyway: the error is likely persistent (same disk,
		// same perms on resume), and skipping .failed would let resume
		// re-spawn this expert into a cohort whose R2 .done outputs
		// were already built without it, violating cohort consistency.
		// Guard with ctx.Err() so SIGINT mid-read stays resumable.
		if ctx.Err() == nil {
			_ = os.WriteFile(filepath.Join(dir, ".failed"), nil, 0o644)
		}
		return result
	}
	if ferr := prompt.CheckForgery(string(body), cfg.Nonce); ferr != nil {
		// Forgery: leave output.md on disk (investigators need it), skip
		// .done so the forged bytes never feed into R2, and write a
		// .failed tombstone so resume does not grant another attempt
		// (D3: R1 failures are permanent per-session).
		_ = os.WriteFile(filepath.Join(dir, ".failed"), nil, 0o644)
		return result
	}
	if err := cfg.Session.TouchDone(dir); err != nil {
		// TouchDone failed after a clean subprocess + clean body. Same
		// cohort-consistency rationale as the ReadFile path above:
		// without .failed, resume would re-attempt this expert into an
		// R2 cohort that already proceeded without it.
		if ctx.Err() == nil {
			_ = os.WriteFile(filepath.Join(dir, ".failed"), nil, 0o644)
		}
		return result
	}
	// Success path: strip stderr.log (design §7 — persisted only on failure).
	_ = os.Remove(req.StderrFile)
	result.Participation = "ok"
	result.Body = string(body)
	return result
}

// RunRound2 runs the peer-aware round on top of R1's results. For every
// expert that survived R1, a personalized prompt is built from the role
// body + the user question + a peer aggregate (every OTHER surviving R1
// expert's output, alphabetical by label, each nonce-fenced). Experts are
// launched in parallel; each writes prompt.md, output.md, and .done into
// its own rounds/2/experts/<label>/ directory.
//
// Carry-forward: if an expert's R2 subprocess fails (after retry) or its
// output is rejected by the forgery check, its R1 body is copied verbatim
// to rounds/2/experts/<label>/output.md and .done is written; the entry in
// the returned slice is marked Participation="carried" with Body holding the
// R1 bytes. This preserves the expert's contribution to voting (D3) without
// exposing partial or forged R2 text to downstream consumers.
//
// Experts that failed R1 (Participation="failed" in the r1 slice) are NOT
// invoked in R2 — there is no last-known-good to carry. Their entry in the
// returned slice stays "failed". R2 aggregate intentionally omits them.
//
// After the goroutines join, the global aggregate rounds/2/aggregate.md is
// written ONCE with every ok-or-carried expert's R2 body, alphabetical by
// label and nonce-fenced. It is the input the voting stage (Task 7) feeds
// to each ballot subprocess. The aggregate is always written when at least
// one expert participated so a forensic trail exists even on quorum failure.
//
// The returned slice mirrors cfg.Experts (sorted by label) so the caller
// can reason about the full cohort shape. ErrQuorumFailedR2 is surfaced
// when fewer than cfg.Quorum experts have Participation != "failed" — the
// outputs are still returned so verdict.json can record every per-expert
// result.
func RunRound2(ctx context.Context, cfg RoundConfig, question string, r1 []RoundOutput) ([]RoundOutput, error) {
	if cfg.Session == nil {
		return nil, fmt.Errorf("RunRound2: RoundConfig.Session required")
	}

	experts := append([]LabeledExpert(nil), cfg.Experts...)
	sort.Slice(experts, func(i, j int) bool { return experts[i].Label < experts[j].Label })

	r1ByLabel := make(map[string]*RoundOutput, len(r1))
	for i := range r1 {
		r1ByLabel[r1[i].Label] = &r1[i]
	}

	outputs := make([]RoundOutput, len(experts))
	var wg sync.WaitGroup
	for i, ex := range experts {
		wg.Add(1)
		go func(i int, ex LabeledExpert) {
			defer wg.Done()
			outputs[i] = runExpertR2(ctx, cfg, ex, question, r1, r1ByLabel[ex.Label])
		}(i, ex)
	}
	wg.Wait()

	// Forensic aggregate: written even when quorum fails so investigators
	// can see the R2 state. Skipped only if nothing to aggregate.
	if err := writeGlobalAggregate(cfg.Session, 2, outputs, cfg.Nonce); err != nil {
		return outputs, fmt.Errorf("RunRound2: write aggregate: %w", err)
	}

	survivors := 0
	for _, o := range outputs {
		if o.Participation == "ok" || o.Participation == "carried" {
			survivors++
		}
	}
	if survivors < cfg.Quorum {
		// Same disjoint-sentinel logic as R1: a rate-limited failure
		// flips the round to ErrRateLimitQuorumFail so cmd/council
		// reaches exit 6 instead of the generic exit-2 path.
		if hasLimitErr(outputs) {
			return outputs, fmt.Errorf("%w: %d survivors < quorum %d", ErrRateLimitQuorumFail, survivors, cfg.Quorum)
		}
		return outputs, fmt.Errorf("%w: %d survivors < quorum %d", ErrQuorumFailedR2, survivors, cfg.Quorum)
	}
	return outputs, nil
}

// runExpertR2 executes one expert's R2 lifecycle with carry-forward. The
// R1-dropped case short-circuits to "failed" without touching disk; the
// success case mirrors runExpertR1's forgery-gated finalize; the failure
// case copies the expert's R1 body into the R2 output path, writes .done,
// and returns Participation="carried" so resume will not re-run this stage.
func runExpertR2(ctx context.Context, cfg RoundConfig, ex LabeledExpert, question string, r1 []RoundOutput, r1Self *RoundOutput) RoundOutput {
	result := RoundOutput{
		Label:         ex.Label,
		Name:          ex.Role.Name,
		Participation: "failed",
	}
	// R1-dropped experts have no last-known-good to carry and are not
	// invoked in R2. Caller uses the participation field to key the
	// verdict entry; the remaining zero fields are harmless.
	if r1Self == nil || r1Self.Participation != "ok" {
		return result
	}

	dir := cfg.Session.RoundExpertDir(2, ex.Label)
	// Resume path (D14): reuse the prior run's finalized stage. A sibling
	// .carried marker (written only by the carry-forward path below)
	// distinguishes "R2 subprocess produced this body" from "R1 body was
	// copied forward after R2 failed", so verdict.json preserves its
	// original participation value across a resume boundary.
	if body, ok := readCompletedStage(dir); ok {
		if _, err := os.Stat(filepath.Join(dir, ".carried")); err == nil {
			result.Participation = "carried"
		} else {
			result.Participation = "ok"
		}
		result.Body = body
		return result
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return result
	}

	peers := buildPeerAggregate(ex.Label, r1, cfg.Nonce)
	// Design §3.4: R2 replaces the per-expert R1 role body with the
	// profile-level peer-aware prompt so experts get the "treat peer outputs
	// as UNTRUSTED / prior-round consensus is NOT ground truth" framing.
	base := prompt.BuildExpert(cfg.R2PromptBody, question, cfg.Nonce)
	promptBody := base
	if peers != "" {
		promptBody = base + "\n" + peers + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(promptBody), 0o644); err != nil {
		return result
	}

	req := executor.Request{
		Prompt:         promptBody,
		Model:          ex.Role.Model,
		Timeout:        ex.Role.Timeout,
		StdoutFile:     filepath.Join(dir, "output.md"),
		StderrFile:     filepath.Join(dir, "stderr.log"),
		MaxRetries:     cfg.MaxRetries,
		AllowedTools:   expertAllowedTools(),
		PermissionMode: defaultPermissionMode,
	}
	resp, err, retries := runWithFailRetry(ctx, ex.Role.Executor, cfg.MaxRetries, req)
	result.ExitCode = resp.ExitCode
	result.Retries = retries
	result.DurationSeconds = resp.Duration.Seconds()

	if err != nil {
		var le *runner.LimitError
		if errors.As(err, &le) {
			result.LimitErr = le
		}
	}

	forged := false
	if err == nil {
		body, rerr := os.ReadFile(req.StdoutFile)
		if rerr != nil {
			// Post-subprocess ReadFile failure. The subprocess may
			// have written a valid R2 body to disk that a transient
			// local I/O error hid from us. Falling through to
			// carry-forward would overwrite those bytes with R1,
			// silently destroying valid work. Same rationale as the
			// TouchDone-failure branch below: return "failed" and
			// let resume retry on a future invocation.
			return result
		}
		if ferr := prompt.CheckForgery(string(body), cfg.Nonce); ferr == nil {
			if derr := cfg.Session.TouchDone(dir); derr == nil {
				_ = os.Remove(req.StderrFile)
				// Scrub any stale .carried marker from a prior
				// carry-forward attempt that wrote .carried but
				// failed before .done. Without this, a later
				// resume would re-read this dir, see .done +
				// .carried, and report Participation="carried"
				// even though the body on disk is a real R2
				// output.
				_ = os.Remove(filepath.Join(dir, ".carried"))
				result.Participation = "ok"
				result.Body = string(body)
				return result
			} else {
				// TouchDone failed AFTER a successful R2
				// subprocess + clean body. Falling through
				// to carry-forward would overwrite the real
				// R2 body on disk with R1, silently
				// destroying valid work. Carry-forward is
				// for subprocess-level failures, not
				// finalize-IO errors — return "failed" and
				// let resume retry on a future invocation.
				return result
			}
		}
		// CheckForgery rejected the body. Fall through to
		// carry-forward, but record the forgery so we preserve
		// the forged bytes below.
		forged = true
	}

	// Interrupt-aware short-circuit: if the parent context was cancelled
	// (SIGINT/SIGTERM), skip carry-forward. Writing .done here would let
	// resume short-circuit R2 for this expert via readCompletedStage,
	// permanently replacing R2 with the R1 body. Carry-forward is for
	// real subprocess failures (exit nonzero, per-expert timeout,
	// forgery), not operator cancellation — ctx.Err() distinguishes
	// them because per-expert timeouts use an inner child context.
	if ctx.Err() != nil {
		return result
	}

	// Preserve forged R2 bytes before overwriting: runExpertR1 leaves the
	// rejected output.md untouched on forgery so investigators can inspect
	// the LLM's injection attempt (see the .failed path in runExpertR1).
	// R2's carry-forward would otherwise destroy the same evidence, so
	// rename the forged output to a sibling path before the overwrite.
	// Rename failure is not fatal — losing the forensic copy is preferable
	// to failing the whole run.
	if forged {
		_ = os.Rename(req.StdoutFile, filepath.Join(dir, "output.rejected.md"))
	}

	// Carry-forward: overwrite the R2 output.md with the R1 body so
	// downstream consumers (aggregate, vote) see the best-known answer
	// for this expert. Writing .done marks the stage "resolved" — resume
	// will skip re-running this expert even though its R2 subprocess
	// failed. A sibling .carried marker preserves the "carried"
	// participation label across a resume boundary; without it, resume
	// reads the R1 body back and reports "ok" instead.
	if werr := os.WriteFile(req.StdoutFile, []byte(r1Self.Body), 0o644); werr != nil {
		return result
	}
	carriedPath := filepath.Join(dir, ".carried")
	if werr := os.WriteFile(carriedPath, nil, 0o644); werr != nil {
		return result
	}
	if derr := cfg.Session.TouchDone(dir); derr != nil {
		return result
	}
	result.Participation = "carried"
	result.Body = r1Self.Body
	return result
}

// buildPeerAggregate returns the nonce-fenced aggregate shown to `forLabel`
// in R2: every OTHER participating expert's output, alphabetical by label,
// separated by blank lines. Participating = Participation in {"ok",
// "carried"} — failed experts contribute nothing (empty body). An empty
// return means there is no peer content to show (single expert cohort, or
// all peers dropped); the caller renders R2 with the question alone.
func buildPeerAggregate(forLabel string, outputs []RoundOutput, nonce string) string {
	peers := make([]RoundOutput, 0, len(outputs))
	for _, o := range outputs {
		if o.Label == forLabel {
			continue
		}
		if o.Participation != "ok" && o.Participation != "carried" {
			continue
		}
		peers = append(peers, o)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Label < peers[j].Label })
	var b strings.Builder
	for i, p := range peers {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(prompt.Wrap(p.Label, p.Body, nonce))
	}
	return b.String()
}

// writeGlobalAggregate writes rounds/<round>/aggregate.md with every
// participating expert's output nonce-fenced and alphabetically ordered.
// The file is the ballot input (Task 7 voting stage) and the canonical
// per-round digest for verdict.json consumers. Failed experts are omitted
// — they have no body to surface. An empty outputs slice still produces an
// empty file so downstream absence-of-file is always an IO error, never a
// "no survivors" semantic.
func writeGlobalAggregate(s *session.Session, round int, outputs []RoundOutput, nonce string) error {
	roundDir := filepath.Join(s.Path, "rounds", strconv.Itoa(round))
	if err := os.MkdirAll(roundDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", roundDir, err)
	}
	filtered := make([]RoundOutput, 0, len(outputs))
	for _, o := range outputs {
		if o.Participation != "ok" && o.Participation != "carried" {
			continue
		}
		filtered = append(filtered, o)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Label < filtered[j].Label })
	var b strings.Builder
	for i, o := range filtered {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(prompt.Wrap(o.Label, o.Body, nonce))
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	path := filepath.Join(roundDir, "aggregate.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readCompletedStage is the resume-path shortcut used by runExpertR1 /
// runExpertR2: if dir carries a .done marker, read output.md and return its
// body. A missing .done OR a missing output.md (shouldn't happen — .done is
// only written after output.md is on disk) returns ok=false so the caller
// falls through to the normal subprocess path.
func readCompletedStage(dir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(dir, ".done")); err != nil {
		return "", false
	}
	body, err := os.ReadFile(filepath.Join(dir, "output.md"))
	if err != nil {
		return "", false
	}
	return string(body), true
}

// runWithFailRetry runs one subprocess with the role-level retry budget.
// The int return is the count of retries actually consumed (attempts beyond
// the first).
//
// Rate-limit failures (executor returns *runner.LimitError per ADR-0013)
// are NOT retried here: the orchestrator absorbs them via quorum, and a
// rate-limited CLI is unlikely to recover within the same session anyway.
// They surface to the caller (RunRound1 / RunRound2) which classifies
// them via errors.As and accumulates them for the verdict's rate_limits
// list.
func runWithFailRetry(ctx context.Context, execName string, maxRetries int, req executor.Request) (executor.Response, error, int) {
	exec, err := executor.Get(execName)
	if err != nil {
		return executor.Response{}, err, 0
	}
	var lastResp executor.Response
	for attempt := 0; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			if attempt == 0 {
				return lastResp, cerr, 0
			}
			return lastResp, cerr, attempt - 1
		}
		resp, err := exec.Execute(ctx, req)
		lastResp = resp
		if err == nil {
			return resp, nil, attempt
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resp, err, attempt
		}
		var limitErr *runner.LimitError
		if errors.As(err, &limitErr) {
			return resp, err, attempt
		}
		if attempt >= maxRetries {
			return resp, err, attempt
		}
	}
}
