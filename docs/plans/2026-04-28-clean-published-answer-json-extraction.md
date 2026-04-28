# Clean Published Answer — JSON Extraction from R2

## Overview

The published answer in `verdict.answer` and `output.md` currently contains debate meta-commentary referring to other experts ("Эксперт A корректно указывает...", "Эксперт B справедливо вводит развилку..."). Per ADR-0006/0008, the winner's R2 text is returned verbatim because voting replaced the judge — there is no synthesis stage.

This plan introduces a **fail-closed JSON extraction** approach: every R2 response must end with a fenced JSON block containing a clean `answer` and `citations[]`. The orchestrator extracts the winner's `answer` field and writes that to `output.md` and `verdict.answer`. Raw R2 stays in `rounds/r2/<label>.txt` unchanged.

When extraction fails (no JSON block, malformed JSON, missing/empty `answer`), the orchestrator falls back to writing the raw R2 — strictly today's behavior, no regression.

This is "Option A done right" (per the council session 2026-04-27T21-24-13Z-pleasantly-above-maggot in issue #16): JSON is materially more reliable than the original `--- ANSWER ---` text marker because every vendor CLI is heavily JSON-trained, and the failure mode is fail-closed instead of fail-open.

**Why JSON extraction over winner-rewrite (the council's headline pick):**
- Zero extra LLM calls (vs. 2 extra calls for B + validation pass).
- Zero added latency on critical path.
- No drift risk — extraction does not regenerate; the winner already wrote the clean answer in the same call as their reasoning, so there is no semantic divergence between what voters read and what gets published.
- Simpler implementation (one parser + one write path) vs. two new debate stages, two new prompts, and a validator-honesty problem.

**Risk acknowledged:** the council's "instruction overflow" critique partially applies — the R2 prompt becomes more demanding. Mitigation: (a) JSON is more enforceable than free-text markers across vendors, (b) fail-closed → raw R2 means worst case is exactly today's behavior, (c) we measure parse success rates after rollout and pivot to B + validation if compliance is unreliable.

## Context (from discovery)

- **Issue:** [#16](https://github.com/fitz123/council/issues/16) — full design discussion + council recommendation.
- **Council session:** `.council/sessions/2026-04-27T21-24-13Z-pleasantly-above-maggot/` — 2-1 verdict for B + validation, with structured-extraction promoted by 2/3 R2 responses as the stronger third option.
- **Files involved:**
  - `defaults/prompts/peer-aware.md` — append JSON-tail requirement (current rules at lines 34–36 instruct "no meta-commentary"; that line stays, JSON requirement is additive).
  - `pkg/debate/vote.go` — `SelectOutput` (lines 348–) currently writes winner's R2 verbatim; this is where extraction wires in.
  - `pkg/debate/` — new file `extract.go` for the JSON parser + extraction logic.
  - `pkg/session/verdict.go` — add extraction outcome to verdict.json.
  - `cmd/council/reporter.go` — verbose stream gets a one-line extraction event.
  - `pkg/debate/reporter.go` — stage event for extraction (stream emit).
- **Patterns to follow:**
  - Profile schema additions in `pkg/config/config.go` lines 16–25 if new prompt fields are needed (none for this plan — peer-aware.md already covers it).
  - `.done` markers on per-stage idempotency — extraction is part of `SelectOutput`, which is already idempotent on `output.md` existence; no new marker needed.
  - Table-driven Go tests with subtests (existing pattern in `vote_test.go`, `rounds_test.go`).
  - Verdict.json schema is documented in `pkg/session/verdict.go`; new field documented there.
- **Out of scope explicitly:**
  - Not changing what voters see (ballot prompt continues to receive full R2 — verbosity-bias question is unresolved in literature; do not change on speculation).
  - Not bumping `verdict.json.version` (additive metadata field; existing consumers ignore unknown fields).
  - Not adding a feature flag (per repo convention: single-user, no backward-compat shims, just describe new behavior).

## Development Approach

- **Testing approach:** Regular (write tests alongside code, table-driven — existing project pattern).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
  - Write unit tests for the extractor's success cases.
  - Write unit tests for every fail-closed path (no JSON block, malformed JSON, missing `answer` key, empty `answer` string, JSON without code fence, multiple JSON blocks — extract last).
  - Update `vote_test.go` for the SelectOutput integration.
- **CRITICAL: all tests must pass before starting next task.**
- **CRITICAL: update this plan file when scope changes during implementation.**
- Run `go test ./...` after each change.

## Testing Strategy

- **Unit tests:** extractor parser (table-driven), SelectOutput integration (success + each fallback path), verdict-shape assertions.
- **No e2e:** project does not have UI e2e infrastructure.
- **Manual smoke:** run `council` against a known question after implementation; eyeball that `output.md` is clean and that `rounds/r2/<winner>.txt` still has the full debate text.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs achievable in this repo.
- **Post-Completion** (no checkboxes): manual smoke, parse-rate observation across real sessions, deciding whether to pivot to B + validation.

## Implementation Steps

### Task 1: Update peer-aware.md prompt to require trailing JSON block

- [x] read `defaults/prompts/peer-aware.md` and identify the format-rules section (currently lines 34–36).
- [x] append a new section instructing the expert to end the response with a fenced JSON code block: `{"answer": "<3–8 sentence clean standalone answer; no peer references; preserve URL citations inline>", "citations": ["<url>", ...]}`. Keep the existing "no meta-commentary" line — they reinforce each other.
- [x] explicitly state that the JSON block must be the LAST content in the response, separated from the prose by a blank line, and that prose with peer references is allowed before the JSON block (the prose feeds voters; the JSON feeds the published output).
- [x] write a unit test that loads the prompt file and asserts both (a) the no-meta-commentary line still exists, (b) a fenced JSON example with `"answer"` key is present, (c) the prompt mentions the LAST-content requirement.
- [x] run `go test ./pkg/config/...` — must pass before task 2.

### Task 2: Implement JSON extractor in pkg/debate/extract.go

- [x] create new file `pkg/debate/extract.go` with `ExtractAnswer(raw string) (answer string, status ExtractStatus)`.
- [x] define `ExtractStatus` enum: `ExtractOK`, `ExtractNoJSONBlock`, `ExtractInvalidJSON`, `ExtractMissingAnswer`, `ExtractEmptyAnswer`.
- [x] parse strategy: scan for the LAST fenced code block (` ```json ... ``` ` or generic ` ``` ... ``` ` whose content begins with `{`); decode with `encoding/json`; require `answer` to be a non-empty string after trimming whitespace.
- [x] do NOT validate the `citations` field — defensive parsing only on `answer`. Citations are operator-level metadata; ignore.
- [x] when status is anything other than `ExtractOK`, the returned `answer` is the empty string.
- [x] no logging, no errors — pure function returning (string, status).
- [x] write `pkg/debate/extract_test.go` with table-driven cases:
  - happy path: simple JSON block with `answer` → returns text + OK.
  - happy path: JSON block with `answer` and `citations[]` → ignores citations, returns answer + OK.
  - JSON block with leading/trailing whitespace in `answer` → trims, returns OK.
  - prose with multiple JSON blocks → extracts the LAST one.
  - JSON block without code fence (raw `{...}` at end of text) → must NOT extract; return `ExtractNoJSONBlock` (require fences for unambiguity).
  - empty input → `ExtractNoJSONBlock`.
  - malformed JSON inside fences → `ExtractInvalidJSON`.
  - JSON missing `answer` key → `ExtractMissingAnswer`.
  - JSON with `answer: ""` (empty after trim) → `ExtractEmptyAnswer`.
  - JSON with `answer: null` → `ExtractMissingAnswer` (null treated as missing).
  - JSON with `answer` as a number/array/object → `ExtractMissingAnswer` (require string type).
  - prose containing the literal text "```json" inside a quoted block but no real fence → `ExtractNoJSONBlock`.
- [x] run `go test ./pkg/debate/ -run TestExtract` — must pass before task 3.

### Task 3: Wire extractor into SelectOutput

- [ ] in `pkg/debate/vote.go`, locate the winner-write branch in `SelectOutput` (lines 381–388).
- [ ] before writing `output.md`, call `ExtractAnswer(r.Body)`. If status is `ExtractOK`, write the extracted answer string (with a single trailing newline) to `output.md`. Otherwise, fall back to writing `r.Body` unchanged.
- [ ] do NOT touch the tied-candidates branch — ties already surface multiple files; extraction in that branch is out of scope and can be added later if needed.
- [ ] return the extraction status from `SelectOutput` so the caller can record it in verdict.json (or accept a callback / extend the result struct — pick whichever fits the existing signature with the smallest delta).
- [ ] update `pkg/debate/vote_test.go` to assert: when winner R2 contains a valid JSON tail, `output.md` contains only the extracted answer (not the raw R2). When winner R2 has no JSON tail, `output.md` contains the raw R2 (existing behavior preserved).
- [ ] add a regression test: winner R2 contains malformed JSON → `output.md` is the raw R2, no panic, status reflects the fallback reason.
- [ ] run `go test ./pkg/debate/...` — must pass before task 4.

### Task 4: Record extraction outcome in verdict.json

- [ ] in `pkg/session/verdict.go`, add a top-level field `AnswerExtraction` (struct) to the verdict shape: `{ "status": "ok" | "fallback_no_json" | "fallback_invalid_json" | "fallback_missing_answer" | "fallback_empty_answer", "winner_label": "B" }`. Use `omitempty` on the struct so resumed sessions that predate this field still serialize cleanly.
- [ ] update `pkg/session/verdict_test.go` canonical fixture (`testdata/verdict_canonical.json`) to include the new field on a happy-path verdict.
- [ ] update the verdict-write call sites (orchestrator main path) to populate the field from `SelectOutput`'s returned status.
- [ ] document the field in the verdict.json schema comment block at the top of `pkg/session/verdict.go`.
- [ ] write tests: verdict.json contains `answer_extraction.status: "ok"` on extraction success; contains `"fallback_*"` matching the reason on each fallback path.
- [ ] run `go test ./pkg/session/...` and `go test ./pkg/debate/...` — must pass before task 5.

### Task 5: Verbose-stream event for extraction outcome

- [ ] in `pkg/debate/reporter.go` (or wherever stage events are defined), add an extraction event type carrying the winner label and the extraction status.
- [ ] in `cmd/council/reporter.go`, add a renderer for the new event:
  - On `ExtractOK`: `[hh:mm:ss] extracted clean answer from winner B (claude_expert): N chars`
  - On any fallback: `[hh:mm:ss] extraction fell back to raw R2 from winner B (claude_expert): <reason>`
- [ ] emit the event from `SelectOutput` (or its caller) after the write completes.
- [ ] write tests for `reporter_test.go` covering both lines.
- [ ] run `go test ./...` — must pass before task 6.

### Task 6: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented:
  - Winner's clean answer becomes `verdict.answer` and `output.md` when JSON extraction succeeds.
  - Raw R2 fallback writes the same content as today on every failure path.
  - `rounds/r2/<label>.txt` is unchanged for every expert (always full body).
  - `verdict.json.answer_extraction` records the outcome.
- [ ] verify edge cases are handled (each `ExtractStatus` value has at least one test).
- [ ] run full test suite: `go test ./...` — all green.
- [ ] run linter: `go vet ./...` and any project-specific linter — fix any issues.
- [ ] verify test coverage of `pkg/debate/extract.go` is >= 90% (measure with `go test -cover ./pkg/debate/`).
- [ ] manual smoke: run `council` against a real question and confirm `output.md` no longer contains "Эксперт A/B/C" references on success path.

### Task 7: Update documentation

- [ ] update `README.md`: under "What's new in v2" (or wherever post-v2 changes are listed), add a one-paragraph note that the published answer is now a clean extraction from the winner's R2 JSON tail, with raw R2 preserved in `rounds/r2/`.
- [ ] add ADR `docs/adr/0014-json-extraction-published-answer.md` following the existing ADR template (Status, Context, Alternatives, Decision, Consequences, Compliance, Research, Supersedes/Extends).
  - Status: Accepted (cite session 2026-04-27T21-24-13Z-pleasantly-above-maggot + issue #16).
  - Decision: JSON-tail extraction with fail-closed → raw R2 fallback.
  - Alternatives evaluated: B + validation pass (council's headline), text-marker section format, mechanical regex strip.
  - Compliance fitness function: `jq -e '.answer_extraction.status == "ok" or (.answer_extraction.status | startswith("fallback_"))' verdict.json` returns true on every session.
- [ ] reference issue #16 and the council session in the ADR's "Research" section.
- [ ] no `verdict.json.version` bump (additive change; consumers ignore unknown fields).

*Note: ralphex automatically moves completed plans to `docs/plans/completed/`.*

## Technical Details

### Parser contract

```go
type ExtractStatus int

const (
    ExtractOK ExtractStatus = iota
    ExtractNoJSONBlock
    ExtractInvalidJSON
    ExtractMissingAnswer  // null, missing, wrong type
    ExtractEmptyAnswer    // empty string after trim
)

func ExtractAnswer(raw string) (answer string, status ExtractStatus)
```

### Parse algorithm

1. Find all fenced code blocks in `raw` matching ```` ```(json)?\n([\s\S]*?)\n``` ````.
2. If zero matches → `ExtractNoJSONBlock`.
3. Take the LAST match's content. Trim leading/trailing whitespace.
4. `json.Unmarshal` into `map[string]any`. On error → `ExtractInvalidJSON`.
5. Look up key `answer`. If absent, null, or non-string → `ExtractMissingAnswer`.
6. Trim whitespace from the answer string. If empty → `ExtractEmptyAnswer`.
7. Return (answer, `ExtractOK`).

### Verdict.json shape addition

```json
{
  "version": 2,
  "...": "existing fields unchanged",
  "answer_extraction": {
    "status": "ok",
    "winner_label": "B"
  }
}
```

Status values: `ok | fallback_no_json | fallback_invalid_json | fallback_missing_answer | fallback_empty_answer`. Field is omitted on ties (no winner) and on sessions where `SelectOutput` did not run.

### peer-aware.md changes

Append (do not replace) a section roughly:

```
Output discipline:
- Your response is in two halves separated by a blank line.
- First half: your peer-engaged refinement, citing peers and URLs.
- Second half (LAST in the response, after a blank line): a fenced JSON
  code block with this exact shape:

  ```json
  {
    "answer": "<3-8 sentence clean standalone answer to the original question; no references to peers; preserve URL citations inline>",
    "citations": ["<url>", "<url>"]
  }
  ```

The JSON block is what gets published as the final answer. The prose
above it is what voters use to evaluate engagement and verification.
```

The "No meta-commentary about the debate process itself" line stays — it now applies specifically to the JSON `answer` field.

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only.*

**Empirical observation period:**
- Run 5–10 real council sessions across mixed question types after merge.
- Inspect each `verdict.json.answer_extraction.status`: count `ok` vs `fallback_*`.
- If parse success rate is ≥ 90% across all three vendors → ship as-is.
- If parse success rate is < 90% on any single vendor (Claude / Codex / Gemini) → open a follow-up issue to (a) tune the prompt for that vendor or (b) implement B + validation as the salvage path.

**Manual verification:**
- Eyeball `output.md` of 3–5 sessions to confirm it reads as a clean answer to the original question with no "Expert A/B/C" references.
- Compare `output.md` vs. `rounds/r2/<winner>.txt` on a few sessions to confirm no information loss in the answer field.

**Follow-up work (potential, not in this plan):**
- Apply the same extraction in the tied-candidates path (multi-winner ties) — out of scope here, separate issue.
- Decide whether voters should see the JSON block stripped from candidate text — current behavior keeps it visible; revisit only if vote quality regressions are observed.
