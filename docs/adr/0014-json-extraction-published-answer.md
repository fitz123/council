# ADR-0014: JSON-Tail Extraction for the Published Answer

**Status:** Accepted
**Date:** 2026-04-28
**Extends:** ADR-0006 (synthesis-only judge), ADR-0008 (debate rounds — winner R2 verbatim)
**Cites:** Issue [#16](https://github.com/fitz123/council/issues/16); council session `2026-04-27T21-24-13Z-pleasantly-above-maggot`

## Context

ADR-0006 (as reshaped by ADR-0008) removed the judge: voting picks a winner, and the winner's R2 body is the published answer verbatim. R2 is peer-aware — every expert reads every other expert's R1 and is instructed to engage with it. The result: clean reasoning, but the published answer often carries debate meta-commentary referring to other experts ("Эксперт A корректно указывает...", "Эксперт B справедливо вводит развилку..."). This is correct *for voters* (they need the engagement to evaluate the answer) and wrong *for the published artifact* (a reader of `output.md` did not see Experts A/B/C and has no referent for those names).

The orchestrator already preserves both halves of the artifact: every R2 body lands verbatim in `rounds/2/experts/<label>/output.md`. What's missing is a clean, peer-free version for the session-root `output.md` and `verdict.answer`.

## Decision

Every R2 response must end with a fenced JSON code block carrying a clean `answer` and optional `citations[]`:

```json
{
  "answer": "<3-8 sentence clean standalone answer; no peer references; preserve URL citations inline>",
  "citations": ["<url>", "..."]
}
```

The orchestrator extracts the winner's `answer` field and writes it to `output.md` and `verdict.answer`. Raw R2 stays in `rounds/2/experts/<label>/output.md` unchanged.

**Fail-closed:** if the JSON block is missing, malformed, lacks the `answer` key, or `answer` is empty/non-string, fall back to writing the raw R2 body. The fallback path is exactly today's behavior — there is no regression risk versus the pre-extraction baseline.

The extraction outcome is recorded in `verdict.json.answer_extraction.{status, winner_label}`, where `status ∈ {ok, fallback_no_json, fallback_invalid_json, fallback_missing_answer, fallback_empty_answer}`. This makes parse-success rate a queryable post-hoc metric.

The verbose stream emits a one-line event per session: either `extracted clean answer from winner B (claude_expert): N chars` or `extraction fell back to raw R2 from winner B (claude_expert): <reason>`.

## Alternatives considered

**a) Winner-rewrite pass (council's headline pick — "B + validation").** A second LLM call rewrites the winner's R2 into a clean answer; an optional third call validates fidelity. Rejected as the primary path: 2 extra LLM calls per session (cost + latency on critical path), regenerates the answer (drift risk versus what voters read), introduces a validator-honesty problem. Kept as the documented fallback if extraction parse rates prove unreliable in production.

**b) Text-marker section format (e.g., `--- ANSWER ---` line in the response).** Rejected: free-text markers are weaker than JSON across vendors. CLIs strip whitespace, reformat headings, and occasionally rewrite literal markers; JSON is more enforceable because every vendor is heavily JSON-trained.

**c) Mechanical regex strip of peer references.** Rejected: the meta-commentary surface is open-ended ("Expert A...", "коллега Б...", "the previous expert"). A regex that catches them all also catches benign mentions; one that doesn't catches nothing useful. Generative output requires generative cleanup.

**d) Strip the JSON block from candidate text shown to voters.** Deferred. Current behavior keeps the JSON visible in candidates; verbosity-bias literature is unresolved and we should not change voter inputs on speculation. Revisit only if vote-quality regressions are observed.

**e) Apply extraction in the tied-candidates branch.** Out of scope. Ties are rare; surfacing multiple `output-<label>.md` files already works. Add later if needed.

## Consequences

- `output.md` and `verdict.answer` become clean, peer-free prose on the happy path.
- `rounds/2/experts/<label>/output.md` is unchanged — full R2 with engagement reasoning is preserved for audit.
- Worst case (every fallback path) is exactly today's behavior: raw R2 published.
- The R2 prompt becomes more demanding (the council's "instruction overflow" critique partially applies). Fail-closed → raw R2 mitigates the downside.
- One additive field in `verdict.json`. No `version` bump (consumers ignore unknown fields).
- New verbose-stream event line per session.

## Compliance

| # | Fitness | Concrete check |
|---|---------|----------------|
| F36 | Extraction outcome recorded on every session that runs `SelectOutput` | `jq -e '.answer_extraction.status == "ok" or (.answer_extraction.status \| startswith("fallback_"))' verdict.json` returns true |
| F37 | Raw R2 preserved regardless of extraction outcome | `rounds/2/experts/<winner>/output.md` byte-for-byte equals the winner subprocess body, even when the session-root `output.md` is the extracted answer |
| F38 | Fail-closed on malformed JSON | Hand-crafted R2 with malformed JSON tail → `output.md` contains the raw R2; `answer_extraction.status == "fallback_invalid_json"`; no panic |
| F39 | Fenced-block requirement enforced | R2 ending in raw `{...}` without code fence → `ExtractNoJSONBlock`; raw R2 published |
| F40 | Last fenced block wins | R2 with two `json` fences → extracts the last; voters' candidates unaffected |

## Research

- Issue [#16 — Cleaning the published answer](https://github.com/fitz123/council/issues/16): full design discussion, including the council's recommendation and the dissenting position that became this plan.
- Council session `.council/sessions/2026-04-27T21-24-13Z-pleasantly-above-maggot/`: 2-1 verdict for "B + validation", with structured-extraction promoted by 2/3 R2 responses as the stronger third option. The choice to ship structured-extraction first (instead of B + validation) is documented in the plan's Overview: zero extra LLM calls, zero added latency on critical path, no drift risk between what voters read and what gets published, and a fail-closed worst case.

## Post-completion fitness function

After 5–10 real sessions across mixed question types:

```
jq '.answer_extraction.status' .council/sessions/*/verdict.json | sort | uniq -c
```

Target: `ok` ≥ 90% across all three vendors (Claude / Codex / Gemini). If any single vendor is below 90%, open a follow-up to either (a) tune the prompt for that vendor or (b) implement alternative (a) — the rewrite pass — as the salvage path.

## Status

Accepted. Implementation landed in branch `clean-published-answer-json-extraction` (Tasks 1–7 of `docs/plans/2026-04-28-clean-published-answer-json-extraction.md`). Empirical observation period documented in the plan's Post-Completion section.
