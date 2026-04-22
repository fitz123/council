# ADR-0006: Judge synthesizes only — no debate rounds at MVP

**Status:** Superseded by [ADR-0008 (debate rounds with anonymization + nonce injection)](0008-debate-rounds-anonymization-injection.md).

v1 shipped with single-pass synthesis. v2 introduces multi-round debate (K≥1, default K=2) with blind round 1 + peer-aware round 2, stable anonymization labels, and a per-session nonce on prompt fences. Rounds count comes from the `rounds` field in the single `defaults/default.yaml` profile; there is no `--rounds` CLI flag.

v2 also removes the judge role entirely — the Round 5 simplification (see `docs/design/v2.md` D15) replaced judge-driven synthesis with expert-cast voting, where the winning expert's R2 text is returned verbatim. ADR-0006's "judge synthesizes only" decision is thus doubly reshaped: MVP had the judge; v2 replaces the judge with voting.

## Context

The inspiration (multi-LLM council) implies rounds of debate: experts see each other's answers, argue, then a judge renders a verdict. This adds a state machine (round-N fan-out, consensus detection or ceiling), meaningful latency, and token cost.

Alternatives:

- **Full debate rounds** — rich, but complex state machine; `round → expert (sees prior round) → round+1`; v2 concern.
- **Single-pass synthesis** — experts each answer the original question once, judge reads all answers and produces one synthesis. Simple, acyclic, predictable latency.
- **Judge-driven follow-up** — judge can ask one clarifying question to one expert mid-flight. Middle-ground complexity; requires interactive subprocess dialog.

## Decision

MVP runs a single fan-out pass: each expert answers the original question once, the judge synthesizes. No back-and-forth. Debate rounds become a v2 feature behind a `--rounds N` flag.

## Consequences

- **(+)** Pipeline stays acyclic — the orchestrator is `fan-out → gate → sequence`, nothing more.
- **(+)** Latency is predictable (≈ `max(expert_timeouts) + judge_timeout`), useful for user expectation setting.
- **(+)** Retry logic is local to one subprocess, not entangled with round state.
- **(−)** Experts do not see each other's answers — cross-pollination benefits are lost.
- **(−)** Judge cannot "press" an expert whose answer is under-specified — known MVP ceiling; users who need this today can re-run council with a different profile.
