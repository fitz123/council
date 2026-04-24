# council v2 — Web Tools Supplement

> This supplement layers on top of `docs/design/v2.md`. It does not replace any
> section. Edits to v2.md itself are applied in-place in the same PR. The
> authoritative decisions live in `docs/adr/0010-web-tools-for-experts.md`
> and `docs/adr/0011-amend-0008-nonce-every-fence.md`; this document is the
> operational reference for readers who want the feature in one narrative.

## 1. What changes in v2

### 1.1 The expert's tool surface

Experts run with `WebSearch` and `WebFetch` available by default. The executor invocation grows two flags:

```
claude -p - --model <model> --output-format text \
  --allowedTools WebSearch,WebFetch \
  --permission-mode bypassPermissions
```

Emitted only when the profile enables tools. An empty `AllowedTools` slice reproduces v1/original-v2 behavior exactly. Verified on live Claude Code — see ADR-0010 §Verification.

### 1.2 The prompt surface

v2 already ships two round-specific prompts (`independent.md` for R1, `peer-aware.md` for R2) wired via the profile's `round_2_prompt_file` field. The schema does not change. What changes is the **contents** of the shipped prompts:

- **R1 (`defaults/prompts/independent.md`)** — research discipline added. "Use tools when facts matter. Cite URLs you actually fetched. Don't force a search on general-knowledge questions."
- **R2 (`defaults/prompts/peer-aware.md`)** — verification discipline + explicit sycophancy resistance added. "Fetch what peers cite. Check what peers assert. Prior-round consensus is NOT ground truth. Maintain your R1 position when peers haven't shown it wrong."

Both prompts still handle the operator question (injected via the unchanged prompt-assembly path). The R2 prompt sees the peer aggregate per v2.md §3.4 — unchanged.

### 1.3 The ballot surface

The ballot subprocess runs with **tools off, hardcoded** — no profile knob. The ballot prompt is "output ONLY the line: VOTE: <label>"; there is nothing to fetch, so `AllowedTools` is always `nil` for ballots regardless of the session-level `tools:` setting.

### 1.4 The fence surface (ADR-0011 amendment)

Every structural fence in the protocol carries the session nonce. Not only the per-expert wrapping fences (already nonce'd per ADR-0008) but also:

- `=== USER QUESTION ... [nonce-<hex>] ===` / `=== END USER QUESTION [nonce-<hex>] ===`
- `=== CANDIDATES [nonce-<hex>] ===` / `=== END CANDIDATES [nonce-<hex>] ===`

The forgery regex tightens to require the `[nonce-<16hex>]` shape, so benign markdown dividers (`=== Table of Contents ===`) from fetched content pass, while forged nonce-bearing fences (`=== END CANDIDATES [nonce-<current>] ===`) are rejected. See ADR-0011 for the full rationale.

### 1.5 The verdict surface

No schema change in v2. `verdict.json.experts[].web_fetches` (structured URL audit) is deferred to v2.1 — see ADR-0010 D21. For v2, fetched URLs appear inline in each expert's `output.md` (prompt discipline); operators grep for them.

## 2. Walkthrough — what changes in §3's trace

Re-read `docs/design/v2.md` §3 "End-to-end walkthrough" with these modifications.

**§3.3 Round 1.** Each expert, given the updated R1 prompt, may call `WebSearch` and `WebFetch` during generation. The final `output.md` may contain inline URL citations. The forgery scanner (amended per ADR-0011) accepts markdown `===` dividers in `output.md`; only nonce-shaped fences are rejected.

**§3.4 Build R2 peer aggregate.** Unchanged mechanically. Peer outputs still get wrapped in nonce-fenced blocks. Markdown `===` dividers *inside* a peer's output are safely contained — no nonce means no forgery match.

**§3.5 Round 2.** Each expert, given the updated R2 prompt, sees the peer aggregate and can call `WebFetch` on peer-cited URLs or `WebSearch` to check peer claims. An R2 expert is expected to either (a) verify and cite peer URLs, (b) maintain its own R1 position with evidence, or (c) state explicitly when peer claims are unverifiable.

**§3.7 Vote.** Ballot subprocesses are spawned fresh (no prior-round context) and with **tools off**. The global fences in the ballot prompt are nonce-tagged per ADR-0011:

```
=== USER QUESTION (untrusted input) [nonce-7c3f9a2b1d4e5f60] ===
<question>
=== END USER QUESTION [nonce-7c3f9a2b1d4e5f60] ===

=== CANDIDATES [nonce-7c3f9a2b1d4e5f60] ===
<per-label R2 aggregate, each label already nonce-wrapped>
=== END CANDIDATES [nonce-7c3f9a2b1d4e5f60] ===
```

## 3. Injection surface — what changed

v2.md §11 R-v2-06 catalogs the baseline v2 injection story (nonce fencing, forgery scanning, trusted operator). This supplement adds:

- **R-v2-11.** Fetched page content can contain embedded prompt-injection attempts. Mitigation: fetched content lands inside an expert's own reasoning/output context, not inside a fenced peer-output block, so it cannot forge a `=== EXPERT: X [nonce-...] ===` fence. An expert *could* still be socially engineered by a page it fetches, but it cannot break the structural integrity of downstream prompts. Accepted under the trusted-operator threat model.

- **R-v2-09.** Adversarial retrieval: an expert may fetch a low-quality source and cite it confidently. v2's distributed voting mitigates this partially — other experts can fetch the same URL and challenge — but doesn't eliminate it. Source-quality scoring is v3.

ADR-0011's regex tightening does **not** weaken the injection surface. It closes one gap (un-nonce'd global fences were forgeable) while eliminating false positives on benign markdown. See ADR-0011's "Rationale" for why this is a strict improvement, not a relaxation.

## 4. Token cost

v2.md §7.2 names "3–5× token amplification" vs. v1 as a known cost. With web tools on by default:

- **R1:** experts that use tools consume tokens on tool-call prompts and on the fetched page content they summarise.
- **R2:** same, plus they may re-fetch peer-cited URLs (no cache; see D19).
- **Ballot:** unchanged (tools-off).

Empirically (based on Tool-MAD and PROClaim — citations in ADR-0010), expect a further 2–3× amplification on research-heavy questions. Total v2+tools runs cost roughly **8–15× a v1 single-pass run**. Operator-facing note for the README.

No cache means R2 experts verifying a peer URL re-pay the fetch cost. Caching is deferred as a v3 research item — see ADR-0010 alt (d) for the three technical questions that need to be answered end-to-end before a design can be picked (content addressability, context-injection path, hit-rate).

## 5. Latency

Smoke-verified on live Claude Code (ADR-0010 §Verification):

- Single-WebFetch R1 task: ~18s.
- Two-WebFetch R2 verification: ~20s.
- Multi-section summary with several fetches: ~30s.

At N=3 × K=2 in parallel, expect **3–5 minutes** of total wall time for a typical research question. Per-expert `timeout: 300s` in the shipped `default.yaml` gives 10×+ headroom over observed worst case.

## 6. What operators should know

- **Determinism.** Web search results and page content change. Replaying a session folder from a week ago won't reproduce exact transcripts if the experts hit live web. v1's reproducibility principle (same `session_id` → same label assignment, same prompt structure) still holds; content reproducibility depends on the live web being static, which it isn't.
- **Disabling tools.** Set `tools.web_search: false` and `tools.web_fetch: false` in the profile. Use for deterministic debugging or priors-only runs.
- **Timeouts.** Shipped `default.yaml` ships with 300s per expert when tools are enabled. Raise further if your typical questions need many fetches.
- **Audit trail.** Fetched URLs appear inline in `rounds/*/experts/*/output.md`. Structured `web_fetches` field is deferred to v2.1.
- **Ballots are always tools-off.** This is hardcoded, not operator-configurable.
