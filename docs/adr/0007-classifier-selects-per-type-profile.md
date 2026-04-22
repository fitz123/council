# ADR-0007: LLM classifier selects per-type profile

**Status:** **Superseded** by v2 simplification (Round 5, 2026-04-21). v2 reframed to a unified debate+vote flow; classifier dropped entirely. See `docs/design/v2.md` Round 5 status banner for rationale: the classifier existed to branch between synthesis / vote / factual flows, but all three flows were consolidated into a single debate+vote path when structured machine-readable output was confirmed out-of-scope for v2. With one flow, there is no routing decision for the classifier to make.

This ADR is retained for historical record — the Round 4 reasoning remains valid within its premises (three-flow architecture). The citations in this file (Xiong et al 2024, Wang et al 2022) remain relevant to any future v3 reintroduction of classifier routing.

## Context

v2 adds three distinct flows — synthesis, vote, factual — each with a different round structure, aggregation shape, and judge behavior. Asking the operator to pick the right mode on every invocation is a footgun: the shape of a question often implies its type (open-ended vs discrete-choice vs single-fact), and mistyped mode selection produces silently wrong flows.

Earlier drafts considered a single master profile with mode-specific fields — `debate:`, `voting:`, `factual:` — guarded by a `mode:` key. This created an "ignored fields" problem (each run uses ~⅓ of the profile and silently skips the rest) and an authority question (when mode and config disagree, who wins?).

Alternatives:

- **`--mode synthesis|vote|factual` CLI flag** — puts the type choice in shell history; invisible to `verdict.json` audit; routes the footgun through a different hole.
- **Heuristic classifier** (regex on question shape) — brittle; many false positives and negatives.
- **Single master profile with mode-specific blocks** — rejected for ignored-fields + authority ambiguity.
- **LLM classifier + type-pure profiles** — chosen.

## Decision

Before flow selection, a lightweight LLM call (Haiku, `confidence ≥ 0.75` threshold) reads the question and outputs `{"type": "synthesis"|"vote"|"factual", "confidence": 0.N, "rationale": "..."}`. The orchestrator looks up the matching profile by name — by default `defaults/<type>.yaml` — and runs that profile's flow. Three profiles ship: `synthesis.yaml`, `vote.yaml`, `factual.yaml`, plus a separate `classifier.yaml` for the classifier call itself.

Each profile has a type-pure YAML schema with no "mode-specific ignored fields." Profile name ≡ type.

**Bypass:**
- `--profile <file>` skips the classifier and loads the named profile directly (non-interactive runs, CI, debugging). Custom profile directories (`--profile-dir <dir>`) were evaluated but dropped on YAGNI grounds during Round 4 review — operators who want custom profiles edit `./.council/<type>.yaml` (v1 loader precedence picks these up transparently) or pass `--profile <file>` per-run.

**Verbose mode (`-v`)** prints the classifier's `type`/`confidence`/`rationale` to stderr before debate starts so the operator can Ctrl+C on obviously wrong classification. This is the only classifier-output safety gate — no regex sanity check and no user-confirmation prompt (both considered and dropped on YAGNI grounds in Round 4).

**Retry policy:** no retry on the classifier call (output is lightweight; failure means re-run whole session after fixing). On timeout → exit 4 `classifier_timeout`. On confidence below threshold → exit 4 `classifier_unclear`. Default `classifier.timeout: 30s`.

## Consequences

- **(+)** Zero hidden mode state — the profile name on disk matches the flow that ran, visible in the session folder and `verdict.json`.
- **(+)** Each profile's YAML is self-documenting about what it runs — no "this field is ignored in this mode" footnote.
- **(+)** Adding a new type in v3 (e.g., `ranking`) = new file + classifier prompt update; zero refactor of existing profiles.
- **(+)** Bypass mechanism (`--profile`) keeps the orchestrator usable in CI / scripted contexts where the type is known in advance.
- **(−)** Every invocation pays one Haiku LLM call (~1–2s on a cheap model).
- **(−)** Wrong classification → wrong flow; mitigated by `-v` preview + `0.75` confidence threshold + exit 4 on low confidence.
- **(−)** Classifier and debate packages cannot share state (they run sequentially, pre-debate vs debate) — codified as `pkg/classifier/` separate from `pkg/debate/` (see `docs/design/v2.md` D12).

## Compliance

- **F10** (fitness function): `jq -e '.classifier.type | IN("synthesis","vote","factual")' verdict.json` passes — classifier output schema uses `type` key, not `mode`.
- Default `confidence_threshold: 0.75` in `defaults/classifier.yaml`; operators can raise but not silently lower without an override flag (future).

## Research

- Xiong et al 2024 — LLM self-reported confidence is poorly calibrated in absolute terms. Mitigation: strict-JSON response (malformed → fail loud), threshold 0.75, operator-visible preview at `-v`.
- Wang et al 2022 (self-consistency) — v3 upgrade path if the 0.75 threshold proves too leaky.

## Supersedes

- [ADR-0004 — flat single-file config for MVP](0004-flat-config-mvp.md). v2 replaces the master `default.yaml` with three type-specific profiles + a classifier config. The directory-layout extension sketched in ADR-0004 was not taken — classifier-driven per-type selection made the shared-experts use case obsolete.
