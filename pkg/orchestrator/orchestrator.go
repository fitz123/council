// Package orchestrator is the Go-level glue that runs the council
// pipeline: fan out expert subprocesses in parallel, check quorum, run
// the judge, and write verdict.json atomically.
//
// It is deterministic Go code — no LLM is ever in the decision loop.
// LLMs appear only as subprocesses behind the executor.Executor
// interface; the orchestrator's control flow (retries, quorum, signal
// handling, verdict assembly) is all policy owned by this package.
//
// Retry-ownership split (load-bearing — see docs/design/v1.md §10 and
// pkg/runner's package doc):
//
//   - Rate-limit retries are RUNNER-OWNED. This package never counts or
//     retries rate-limit failures — they are swallowed inside pkg/runner,
//     and if one bubbles up here it means the runner's rate-limit budget
//     was exhausted, which we treat as a terminal failure like any other.
//   - Fail-retries (timeout, non-zero exit) are ORCHESTRATOR-OWNED, driven
//     by profile.MaxRetries. The single shared path for retry + Execute
//     is runWithFailRetry, used by both expert and judge goroutines
//     (satisfies architect-review P2 — no per-role subprocess primitive
//     duplication).
//
// Signal handling: the caller (cmd/council) owns SIGINT/SIGTERM and
// cancels the root context. We respect ctx.Done() — goroutines return
// quickly, and the verdict is still written atomically BEFORE Run
// returns, so cmd/council can exit 130 with the verdict visible on disk.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/prompt"
	"github.com/fitz123/council/pkg/runner"
	"github.com/fitz123/council/pkg/session"
)

// Sentinel errors the orchestrator returns to cmd/council for exit-code
// mapping. The verdict.json is always written before these are returned,
// so callers can exit immediately on receiving them.
var (
	ErrQuorumFailed = errors.New("orchestrator: quorum not met")
	ErrJudgeFailed  = errors.New("orchestrator: judge failed after retry")
	ErrInterrupted  = errors.New("orchestrator: interrupted")
)

// Validate checks that every executor name referenced by the profile is
// registered. Called by cmd/council before Session.Create so a typo like
// `executor: calude-code` fails fast with exit 1 (config error) instead
// of materialising a session folder and surfacing as a runtime expert
// failure (which could even pass quorum if another expert succeeds, or
// show up as exit 3 "judge_failed" for the judge case).
func Validate(p *config.Profile) error {
	var missing []string
	if _, err := executor.Get(p.Judge.Executor); err != nil {
		missing = append(missing, fmt.Sprintf("judge uses unknown executor %q", p.Judge.Executor))
	}
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
// ("2026-04-19T17:02:14Z") — RFC3339 with colon-separated time (the
// session-id timestamp uses dashes for filesystem-safety; this one does
// not because verdict.json is a JSON string, not a path).
const timestampLayout = time.RFC3339

// Run executes one council pipeline end-to-end. It always writes
// verdict.json before returning — on success, on quorum failure, on
// judge failure, and on interruption. The returned VerdictV1 is the same
// one written to disk.
//
// The caller is expected to have already invoked session.Create to
// materialize the session folder and the initial artifacts
// (question.md, profile.snapshot.yaml, rounds/1/{experts,judge}/). Run
// does not create the session folder itself because id/petname
// allocation is not its concern.
func Run(ctx context.Context, profile *config.Profile, question string, sess *session.Session) (*session.VerdictV1, error) {
	startedAt := time.Now().UTC()
	v := &session.VerdictV1{
		Version:     1,
		SessionID:   sess.ID,
		SessionPath: verdictSessionPath(sess.ID),
		Profile:     profile.Name,
		Question:    question,
		StartedAt:   startedAt.Format(timestampLayout),
		Rounds:      []session.Round{{}},
	}

	// Fan out experts in parallel. Each goroutine populates its slot in
	// the results slice; ordering is preserved from profile.Experts so
	// verdict.json lists experts in declaration order.
	results := make([]*session.ExpertResult, len(profile.Experts))
	outputs := make([]prompt.ExpertOutput, len(profile.Experts))
	ok := make([]bool, len(profile.Experts))

	var wg sync.WaitGroup
	for i, ex := range profile.Experts {
		wg.Add(1)
		go func(i int, ex config.RoleConfig) {
			defer wg.Done()
			if ctx.Err() != nil {
				// never-started: leave results[i] == nil so this expert
				// is absent from verdict.json (design: "absent for
				// never-started").
				return
			}
			res, body, success := runExpert(ctx, profile, ex, question, sess)
			results[i] = &res
			if success {
				outputs[i] = prompt.ExpertOutput{Name: ex.Name, Body: body}
				ok[i] = true
			}
		}(i, ex)
	}
	wg.Wait()

	// Collect expert results in declaration order, skipping never-started.
	for _, r := range results {
		if r != nil {
			v.Rounds[0].Experts = append(v.Rounds[0].Experts, *r)
		}
	}

	// Interrupt short-circuit: if context is done, skip quorum + judge
	// and flush an "interrupted" verdict. Any expert that was in-flight
	// has already had its status set to "interrupted" by runExpert; any
	// never-started expert is absent from the slice.
	if ctx.Err() != nil {
		v.Status = "interrupted"
		finalize(v, startedAt)
		if err := sess.WriteVerdict(v); err != nil {
			return v, fmt.Errorf("write verdict: %w", err)
		}
		return v, ErrInterrupted
	}

	// Quorum gate. Strict less-than — `survivors == quorum` passes.
	var survivors []prompt.ExpertOutput
	for i := range outputs {
		if ok[i] {
			survivors = append(survivors, outputs[i])
		}
	}
	if len(survivors) < profile.Quorum {
		v.Status = "quorum_failed"
		finalize(v, startedAt)
		if err := sess.WriteVerdict(v); err != nil {
			return v, fmt.Errorf("write verdict: %w", err)
		}
		return v, ErrQuorumFailed
	}

	// Judge. Runs sequentially — the judge prompt needs all expert
	// outputs assembled before the call starts (see design §3).
	judgeRes, answer, judgeOK := runJudge(ctx, profile, question, survivors, sess)
	v.Rounds[0].Judge = judgeRes
	if !judgeOK {
		// Was this cancellation or failure? Distinguishing the two is what
		// lets cmd/council choose exit 130 vs exit 3.
		if ctx.Err() != nil {
			v.Status = "interrupted"
			finalize(v, startedAt)
			if err := sess.WriteVerdict(v); err != nil {
				return v, fmt.Errorf("write verdict: %w", err)
			}
			return v, ErrInterrupted
		}
		v.Status = "judge_failed"
		finalize(v, startedAt)
		if err := sess.WriteVerdict(v); err != nil {
			return v, fmt.Errorf("write verdict: %w", err)
		}
		return v, ErrJudgeFailed
	}

	v.Answer = answer
	v.Status = "ok"
	finalize(v, startedAt)
	if err := sess.WriteVerdict(v); err != nil {
		return v, fmt.Errorf("write verdict: %w", err)
	}
	return v, nil
}

// finalize sets the end-timestamp and total-duration fields on v using
// the same clock we captured at start. Called from every exit path (ok,
// quorum_failed, judge_failed, interrupted) so verdict.json always has
// meaningful started_at / ended_at / duration_seconds values.
func finalize(v *session.VerdictV1, startedAt time.Time) {
	end := time.Now().UTC()
	v.EndedAt = end.Format(timestampLayout)
	v.DurationSeconds = end.Sub(startedAt).Seconds()
}

func verdictSessionPath(id string) string {
	return "./" + filepath.ToSlash(filepath.Join(".council", "sessions", id))
}

// runExpert handles one expert's full lifecycle: mkdir its stage dir,
// write prompt.md, invoke Executor.Execute via runWithFailRetry, read
// stdout, touch .done on success. The returned body is empty on
// failure/interrupt; the bool says whether the expert output should feed
// into the judge prompt.
//
// Per-expert status semantics:
//   - "ok"          — Execute succeeded, .done touched
//   - "failed"      — Execute failed after all retries, stderr.log kept
//   - "interrupted" — ctx cancelled mid-run, no .done, stderr.log kept
func runExpert(ctx context.Context, profile *config.Profile, ex config.RoleConfig, question string, sess *session.Session) (session.ExpertResult, string, bool) {
	dir := sess.ExpertDir(ex.Name)
	base := session.ExpertResult{
		Name:     ex.Name,
		Executor: ex.Executor,
		Model:    ex.Model,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		base.Status = "failed"
		return base, "", false
	}

	promptBody := prompt.BuildExpert(ex.PromptBody, question, nil)
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(promptBody), 0o644); err != nil {
		base.Status = "failed"
		return base, "", false
	}

	req := executor.Request{
		Prompt:     promptBody,
		Model:      ex.Model,
		Timeout:    ex.Timeout,
		StdoutFile: filepath.Join(dir, "output.md"),
		StderrFile: filepath.Join(dir, "stderr.log"),
		MaxRetries: profile.MaxRetries,
	}
	resp, err, retries := runWithFailRetry(ctx, ex.Executor, profile.MaxRetries, req)

	base.ExitCode = resp.ExitCode
	base.Retries = retries
	base.DurationSeconds = resp.Duration.Seconds()

	if err != nil {
		// Context cancellation is a distinct outcome from fail-after-retry:
		// it maps to status "interrupted" so cmd/council can exit 130 and
		// the verdict distinguishes "we ran out of time" from "the caller
		// told us to stop".
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			base.Status = "interrupted"
		} else {
			base.Status = "failed"
		}
		return base, "", false
	}

	body, rerr := os.ReadFile(req.StdoutFile)
	if rerr != nil {
		base.Status = "failed"
		return base, "", false
	}
	if err := sess.TouchDone(dir); err != nil {
		base.Status = "failed"
		return base, "", false
	}
	// Success — remove stderr file (design §7: persisted only on failure).
	// runner.Run already does this on its own success path, but a stale
	// file from a prior failed attempt that a retry later succeeded after
	// would linger, so we clean up here too.
	_ = os.Remove(req.StderrFile)
	base.Status = "ok"
	return base, string(body), true
}

// runJudge mirrors runExpert but for the single judge stage. It always
// gets one retry on failure (per design §10: "Retry once. If retries
// exhausted → exit 3"), independent of profile.MaxRetries which is an
// expert-level knob.
//
// On success: writes synthesis.md, touches .done, returns the synthesis
// body so the orchestrator can set VerdictV1.Answer. On failure: leaves
// stderr.log behind, does NOT touch .done, and signals the orchestrator
// to flip status to "judge_failed" / "interrupted".
func runJudge(ctx context.Context, profile *config.Profile, question string, experts []prompt.ExpertOutput, sess *session.Session) (session.JudgeResult, string, bool) {
	dir := sess.JudgeDir()
	result := session.JudgeResult{
		Executor: profile.Judge.Executor,
		Model:    profile.Judge.Model,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return result, "", false
	}

	promptBody := prompt.BuildJudge(profile.Judge.PromptBody, question, experts, nil)
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(promptBody), 0o644); err != nil {
		return result, "", false
	}

	req := executor.Request{
		Prompt:     promptBody,
		Model:      profile.Judge.Model,
		Timeout:    profile.Judge.Timeout,
		StdoutFile: filepath.Join(dir, "synthesis.md"),
		StderrFile: filepath.Join(dir, "stderr.log"),
		MaxRetries: profile.MaxRetries,
	}
	const judgeMaxRetries = 1
	resp, err, retries := runWithFailRetry(ctx, profile.Judge.Executor, judgeMaxRetries, req)

	result.ExitCode = resp.ExitCode
	result.Retries = retries
	result.DurationSeconds = resp.Duration.Seconds()

	if err != nil {
		return result, "", false
	}
	body, rerr := os.ReadFile(req.StdoutFile)
	if rerr != nil {
		return result, "", false
	}
	if err := sess.TouchDone(dir); err != nil {
		return result, "", false
	}
	_ = os.Remove(req.StderrFile)
	return result, string(body), true
}

// runWithFailRetry is the shared Execute-with-retry path used by every
// subprocess call in this package (both expert and judge). Consolidating
// retry logic here prevents drift between role-specific paths and gives
// architect-review P2 a single grep-able call site.
//
// It retries up to maxRetries times on non-cancellation errors. Context
// cancellation (or deadline) short-circuits — the caller handles that by
// inspecting the returned error with errors.Is.
//
// The returned Response reflects the LAST attempt only (not a cumulative
// sum across retries). verdict.json's per-role duration_seconds is
// defined as the successful attempt's wall-clock; on failure it is the
// final failed attempt's wall-clock. The int return is the number of
// orchestrator-level retries — i.e., attempts beyond the first.
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
		// Rate-limit retries are runner-owned (see package doc). If
		// ErrRateLimit bubbles up here the runner's budget was already
		// spent; retrying at the orchestrator layer would stack a second
		// budget on top of the first and is explicitly excluded by
		// plan §Task 6 + design §10.
		if errors.Is(err, runner.ErrRateLimit) {
			return resp, err, attempt
		}
		if attempt >= maxRetries {
			return resp, err, attempt
		}
	}
}
