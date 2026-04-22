# ADR-0009: Pairwise tournament with self-defense as conditional tie-break

**Status:** **Superseded** by v2 simplification (Round 5, 2026-04-21). v2 reframed to a unified debate+vote flow where voting is the sole aggregation mechanism; the tournament + self-defense layer was removed. On a three-way tie at N=3, v2 surfaces all tied outputs to the operator instead of triggering a tournament (per design doc D16, YAGNI). See `docs/design/v2.md` Round 5 status banner for rationale.

This ADR is retained for historical record — the Round 4 reasoning (Irving et al 2018 two-agent debate framework, Panickssery 2024 self-preference bias caveat) remains valid research should v3 reintroduce a close-vote argumentation layer.

## Context

The vote flow (see ADR-0007) aggregates expert ballots into a tally and picks a winner. Close votes — where the winner gets ≤60% of total votes — are the cases where a judge polishing the tally winner is most likely to be wrong: two candidates are close in merit, and a 60/40 split may not reflect the better answer. (Each expert casts one unweighted vote; weighted voting was dropped on YAGNI grounds in Round 4.)

For these cases a second argumentation layer is helpful: let each top candidate's advocate make a case for its answer, then a tournament judge picks a winner based on the argument quality.

Alternatives:

- **Always polish the tally winner** (no tournament) — drops argumentation on close calls; the 60/40 edge case was exactly the reason the vote flow was introduced.
- **Cross-defense** (A defends B's candidate, B defends A's) — no research supports cross-defense; cognitively strange for the defender.
- **Neutral defenders** (spawn fresh experts who did not participate) — wastes a subprocess slot; for N=2 it burns one of the two existing experts anyway.
- **Self-defense** (original author defends its own candidate) — used in Irving et al 2018's two-agent + judge framework. Honest about the argumentation cost: the original author knows its reasoning best. Self-preference bias (Panickssery et al 2024) accepted and documented.

## Decision

When the vote winner gets **≤60%** of total votes, a tournament runs between the top-2 candidates:

1. Each candidate is defended by the expert that originally produced it.
2. A tournament judge reads both defenses and picks the winner.
3. The final judge polishes the tournament winner into the user-facing answer.

**Tournament judge prompt** refers to **candidates**, not experts: "Defender of Candidate A1" / "Defender of Candidate A2". No session-stable labels (no "Expert A"), no disclosure that self-defense is happening. Preserves the anonymization boundary from ADR-0008.

**Edge case — both top-2 candidates came from the same expert:** the tournament is **skipped** and the judge polishes the tally winner directly (single-source → no meaningful pairwise argumentation).

**Tournament judge timeout:** folds into `judge.timeout` unless `vote.yaml` sets `voting.tournament_judge.timeout` as an override (the nested form matches the rest of the v2 vote-profile schema). The defender subprocess timeouts come from the defender's own `expert.timeout` field (defenders ARE the source experts) — no separate `defender_timeout` config knob (dropped Round 4).

**Tournament failure** (`tournament_judge_failed`): exit 0 with a lexicographic fallback winner (v1-style best-effort; consistent with ADR-0005's lenient quorum posture — partial answer beats aborted run).

**Threshold:** default `voting.tournament_trigger_threshold: 0.60` in `vote.yaml`. Operators can raise or lower it per-profile.

## Consequences

- **(+)** Close votes (≤60%) get a second argumentation pass — not just a mechanical polish of the tally.
- **(+)** Single-source edge case handled cleanly: tournament skipped, no wasted subprocess.
- **(+)** Tournament judge prompt is candidate-oriented, not expert-oriented — anonymization (ADR-0008) and mode separation preserved.
- **(+)** Failure degrades gracefully (lexicographic fallback, exit 0) — debate never deadlocks on tournament failure.
- **(−)** Self-preference bias (Panickssery et al 2024) means the defender may argue its own position slightly more persuasively than a neutral evaluator would. Acknowledged; deferred to v3 via optional `--tournament-mode cross|neutral` flag.
- **(−)** Close votes add one additional pairwise-judge LLM call — latency + token cost on the cases that hit the threshold. Accepted as the price of argumentation depth where it matters.
- **(−)** The 60% threshold is a judgment call, not a researched optimum. Mitigated by per-profile override.

## Compliance

- **F11** (fitness function): tournament-trigger arithmetic uses threshold inequality, not float equality. Mock tally `{"A1":4,"A2":2,"A3":2}` → winner_share = 0.5 ≤ 0.60 → tournament triggers; `jq -e '.tournament != null and (.voting.winner_share <= 0.60)'` passes.
- Inverse fitness: winner_share > 0.60 → no tournament → `jq -e '.tournament == null and (.voting.winner_share > 0.60)'`.

## Research

- Irving et al 2018 — original two-agent + judge tournament framework.
- Panickssery et al 2024 — self-preference bias documented; accepted at v2.

## Relation to other ADRs

- Depends on [ADR-0007 (classifier selects per-type profile)](0007-classifier-selects-per-type-profile.md): tournament is a step inside the `vote` flow only.
- Depends on [ADR-0008 (anonymization + nonce)](0008-debate-rounds-anonymization-injection.md): tournament judge prompts are nonce-fenced; candidate labels (A1/A2) reuse the anonymization scheme.
