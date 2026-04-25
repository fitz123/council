# ADR-0005: Single model/CLI at MVP; multiple expert personas

**Status:** Superseded by ADR-0012 (2026-04-25).

> v3 lifts the single-CLI constraint this ADR preserved through v1
> and v2; it also relaxes validation to allow `len(experts) == 1`
> (so `council init` can write a working profile when only one CLI
> is authed on the host). See `docs/adr/0012-multi-cli-executors.md`
> for the cross-vendor executor design and `docs/plans/2026-04-25-v3-multi-cli.md`
> Task 7 for the validator change. The text below is retained for
> historical continuity and for the original MVP rationale for
> preferring N≥2 expert profiles — a preference v3 honors in its
> default `council init` output (quorum = min(2, N)) even though
> the validator no longer enforces N≥2 as a hard invariant.

## Context

The inspiration (a YouTube-described multi-LLM council) runs 4-5 experts in parallel across different models. An earlier draft of this ADR took the "restraint" of MVP to mean "one expert plus a judge" — the smallest possible committee. That framing did not hold up:

- **A single-expert MVP does not demonstrate the council idea.** The judge has no synthesis to do (one opinion is trivially "the synthesis"); the quorum gate is not exercised in any non-degenerate shape; the value proposition of "fan out to N, reconcile via judge" is invisible.
- **The operational cost that *actually* matters at MVP is the *CLI/model* count**, not the expert count. Rate-limit pressure, auth plumbing, and the executor-surface verification step are all per-CLI, not per-expert. Running two personas through the same `claude -p` subscription costs roughly the same as running one.
- **The "single expert exercises fan-out code" bullet was optimistic.** Partial-failure quorum (e.g. 1 of 2 fails) is exactly the code path that matters, and N = 1 cannot exercise it.

Alternatives:

- **Single expert, single CLI (rejected, per above).** Hides the value proposition.
- **Multiple experts, multiple CLIs from day 1.** Closer to the inspiration, but adds executor-verification and auth plumbing work for Codex and Gemini on top of Claude Code before the architecture has shipped once.
- **Multiple experts, single CLI (chosen).** Different personas through the same underlying `claude -p` subscription. The judge performs real synthesis from day 1; quorum behavior is meaningfully exercised; `Executor` interface proves it can host a single implementation cleanly before being asked to host two.

## Decision

The MVP default profile has **N ≥ 2 expert personas** served by **one** underlying CLI (`claude-code`). The default `profile.yaml` ships two personas: `independent` (direct substantive answer) and `critic` (surface counter-arguments, hidden assumptions, failure modes). Both use the same `executor: claude-code` and the same `model: sonnet`; only the `prompt_file` differs. Quorum defaults to `1` (lenient — one survivor is enough for the judge to synthesize).

The constraint that stays "single" at MVP is the *CLI/model* dimension: Claude Code only. v2 adds `executor: codex` and `executor: gemini-cli` — one new file per executor in `pkg/executor/`, zero orchestrator changes.

## Consequences

- **(+)** Judge does real synthesis from day 1 — not just format normalization. The council idea is visible in the MVP demo.
- **(+)** Quorum logic is exercised under partial failure (1 of 2 succeeds → judge still synthesizes).
- **(+)** Personas can be tuned independently via `prompt_file` without touching code or adding new CLIs.
- **(+)** Executor-verification work (flag surface, rate-limit handling) stays scoped to one CLI for MVP.
- **(−)** Rate-limit pressure is higher than with a single expert — two parallel calls against one subscription. Mitigated by the 429-distinct-from-timeout classification in `docs/design/v1.md` §10.
- **(−)** Two personas shipping in defaults means two prompt files to maintain. Small cost; offset by the benefit of the MVP actually demonstrating synthesis.
