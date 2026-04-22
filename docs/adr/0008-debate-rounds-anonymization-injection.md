# ADR-0008: Debate rounds with blind R1, stable anonymization, per-session nonce

**Status:** Accepted (Round 4 sign-off, 2026-04-21). Extends ADR-0005; supersedes ADR-0006.

## Context

ADR-0006 deferred debate rounds to v2. Multi-round debate between experts demands three design choices, each with a meaningful alternative:

1. **Round 1 shape** — can experts see peers in R1, or is R1 blind?
2. **Identity across rounds** — do labels stay stable, rotate, or use named roles?
3. **Injection boundary** — how do we prevent expert output from breaking out of its wrapper and injecting into the judge's task block or a later round's aggregate?

Expert output is LLM-generated text. Plain ASCII delimiters (v1 style: `=== EXPERT: A ===`) are forgeable — an expert can emit those literal strings as part of its answer, intentionally or by accident, and corrupt downstream prompt construction.

Alternatives:

- **Peer-aware round 1** (sequential) — rejected on wall-time grounds: N=3..5 experts running sequentially dominates latency.
- **Per-round label rotation** — harder to fingerprint but confuses debate semantics ("Expert B said X in R1, Y in R2 — why?") and debug logs.
- **Named roles** ("Engineer", "Ethicist") — leaks role information into expert prompts, breaking the "all experts get the same prompt" invariant.
- **Plain ASCII fences (v1-style)** — no forgery defense.
- **Per-section SHA-256 fences** — over-engineered under trusted-operator threat model; noise in logs. Deferred to v3.
- **Blind R1 + stable labels + per-session nonce** — chosen.

## Decision

**R1 is blind; R2+ is peer-aware.** All experts run round 1 in parallel with no visibility into peer outputs. From round 2 onward, each expert receives an aggregate of the previous round's peer outputs (excluding itself), ordered alphabetically by anonymized label to keep prompts identical across reruns.

**Labels are stable per session.** "Expert A", "Expert B" refer to the same real expert throughout one run. The label→real-name map is derived deterministically by hashing `session_id` (SHA-256, first 8 bytes → `math/rand/v2` PCG), stored only in `verdict.json.anonymization`, and revealed to humans post-hoc.

**Label alphabet:** single letters `A`, `B`, `C`, …, `Z`. Scales up to N=26 at worst; v2 defaults N=3 so single letters suffice. For N>26, the obvious extension `A1, A2, …` is available — Round 5 simplification removed vote-path candidate IDs so there is no longer a collision concern. Deferred to v3 when multi-expert configurations beyond 26 become relevant.

**Per-session 16-hex nonce** (8 bytes of entropy from `crypto/rand`) tags every prompt fence in the session:

```
=== EXPERT: A [nonce-7c3f9a2b1d4e5f60] ===
...
=== END EXPERT: A [nonce-7c3f9a2b1d4e5f60] ===
```

All LLM-sourced text going into a downstream prompt is fence-wrapped, and the wrapped content is scanned for the session nonce as a substring AND for ANY line-anchored delimiter pattern `(?m)^=== .* ===$` (including open fences like `=== EXPERT: A [nonce-…] ===`, close fences like `=== END EXPERT: A [nonce-…] ===`, and global section fences like `=== CANDIDATES ===` / `=== END CANDIDATES ===`). A match on either check rejects the output (forgery detection). The broad delimiter regex is deliberate — without the session nonce an attacker cannot forge an open fence, but they could still emit a closing or global delimiter to confuse downstream prompt parsing. Rejecting all delimiter-shaped lines from LLM-sourced content closes that gap.

**Nonce scope (Round 5 v2):** fence ALL LLM-derived text going into a downstream prompt:
- Expert R1 outputs when building R2 peer aggregates.
- Expert R2 outputs when building the global aggregate that feeds the vote ballot prompt.

Operator's question text is NOT fenced (trusted-operator threat model) but IS sanity-scanned at load time with the same delimiter regex; a match exits with `status: injection_suspected_in_question`, exit 1.

**Historical (Round 4, superseded):** earlier drafts of this ADR also fenced judge prompts, tournament-judge prompts, factual-unanimity-judge prompts, and classifier rationale. Those components were removed in Round 5 (see superseded ADR-0007 and ADR-0009); the nonce scope above reflects the current v2 design with only debate rounds + vote remaining.

**Future / v3 — per-round position shuffle seed derivation (not used in v2):** v2 peer aggregates are ordered alphabetically by anonymized label for determinism; there is no shuffle in v2. If a future v3 experiment reintroduces position shuffling, use `sha256(session_id || round_num || self_label)` → first 8 bytes → `rand.NewPCG(seedHi, seedLo)` so shuffles remain reproducible for debugging. The seed formula is documented here so the v3 implementer doesn't reinvent it.

## Consequences

- **(+)** Debate rounds work without self-preference or recency-bias leakage across rounds — labels are stable; peer-aggregate ordering is deterministic.
- **(+)** Same session reruns produce identical prompts given identical expert outputs — reproducibility for debugging.
- **(+)** Forgery via plain-ASCII fences is stopped by nonce verification — an attacker needs the session's nonce to forge a convincing fence.
- **(+)** Operator's question stays un-fenced (no user-visible noise) while still being load-time-sanity-scanned.
- **(−)** Writing-style leak — an expert's signature phrasing may reveal its real identity across rounds (e.g., specific Claude phrasings). Accepted as known limitation (R-v2-06); per-expert style anonymization deferred.
- **(−)** Blind R1 means experts cannot cross-pollinate in round 1 — diversity of initial drafts is the gain, richness of R1 is the cost. Acceptable given R2+ is peer-aware.
- **(−)** Under a trusted-operator threat model, the 16-hex nonce is sufficient; in an untrusted-executor future (v3 with multi-vendor CLIs) per-section SHA-256 hashes may be needed. Documented upgrade path.

## Compliance

- **F8** (fitness function): `verdict.json.anonymization` resolves to the same real name for every occurrence of a label across all rounds — `jq -e '.anonymization as $map | [.rounds[].experts[] | {label, real_name}] | unique | all(. as $e | $map[$e.label] == $e.real_name)'`.
- **F5** (fitness): SIGINT mid-run produces partial `verdict.json` with the anonymization map intact.
- **F9 (proposed)**: forge an expert output containing a fake fence without the session nonce → orchestrator rejects the output and marks the expert as failed for that round.

## Research

- Du et al 2024 — blind-first + peer-aware-downstream pattern; empirical gains on reasoning benchmarks come from the R2+ peer-aware phase.
- ReConcile — stable-identity viability at N=3 for reasoning tasks.
- Dalkey 1969 (Delphi) — classical authority on anonymized expert panels.
- Panickssery et al 2024 — self-preference bias, accepted as known limitation.

## Supersedes

- [ADR-0006 — judge synthesizes only, no debate rounds at MVP](0006-synthesis-only-judge.md). The single-pass v1 design is replaced in v2 by a debate+vote flow under the single `defaults/default.yaml` profile. The anticipated `--rounds N` flag in ADR-0006 was reshaped into a profile-level `rounds:` field (default K=2; K∈{1,2} allowed), rather than a CLI flag.

## Extends

- ADR-0005 — v2 raises the N default to 3 in the single profile, adds validation `len(experts) >= 2`. The "single CLI, multiple personas" thesis stays in force; v2 just tightens the minimum and adds a vote stage after the final round.
