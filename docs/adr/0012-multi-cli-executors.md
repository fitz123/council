
# ADR-0012: Multi-CLI Executors (Codex + Gemini)

**Status:** Proposed
**Date:** 2026-04-24
**Supersedes:** ADR-0005 (single-CLI-multiple-personas MVP constraint)
**Depends on:** ADR-0010 (web tools for experts — the `AllowedTools` field
flowing from `pkg/debate/rounds.go` is the integration point), ADR-0011
(nonce every structural fence — no direct interaction but implementation
lands on top of it)

## Context

ADR-0005 (v1 MVP) shipped with one CLI for per-CLI verification
economy. It forecast `v2 adds executor: codex and executor: gemini-cli`.
v2 shipped in PR #4 still claude-only; v2.md §1.4 retains the
single-vendor caveat. This ADR executes the forecast.

Zhang et al. 2025 ("Stop Overvaluing Multi-Agent Debate",
arXiv:2502.08788) found MAD beats single-agent baselines *primarily*
when model heterogeneity is present. v2's three Claude Sonnet experts
are three samples of one distribution; R1 disagreement is correlated,
R2 refinement is self-consensus-prone. Cross-vendor lineages (Anthropic
/ OpenAI / Google) give independent training corpora, restoring the
Condorcet independence the voting stage implicitly assumes.

**Webfetch compatibility is a hard requirement.** PR #5 (ADR-0010 +
ADR-0011) ships web tools hardcoded in `pkg/debate/rounds.go`:
`AllowedTools = ["WebSearch", "WebFetch"]` on every expert spawn. For
the cross-vendor debate to carry the web-augmented quality that PR #5
delivers, codex and gemini experts must also fetch/search live — not
just answer from priors. A priors-only codex/gemini would silently
regress the web-tool quality gains PR #5 introduces, exactly on the
cross-vendor peer signal the debate relies on.

## Decision

Add `pkg/executor/codex/` and `pkg/executor/gemini/`. Each implements
the existing `Executor` interface; each registers via `init()`. Each
translates the webfetch `AllowedTools` / `PermissionMode` fields to
its native CLI surface so live web tools reach every expert.

**Interface extension** — one new method:

```go
type Executor interface {
    Name() string
    BinaryName() string  // for exec.LookPath in council init + preflight
    Execute(ctx context.Context, req Request) (Response, error)
}
```

**Model naming (A2):** literal CLI model IDs in profile YAML;
`MapModel` is identity for every executor. `Request.Model` doc:
"literal CLI `--model` flag value, verbatim." Claude CLI aliases
`sonnet`/`opus`/`haiku` server-side so profiles using those
aliases work without translation.

**Per-CLI AllowedTools translation:**

| Canonical (webfetch constant) | claude-code | codex | gemini-cli |
|---|---|---|---|
| `WebSearch` | passed verbatim to `--allowedTools WebSearch,WebFetch` | triggers `-c tools.web_search=true` (once per call, regardless of whether fetch is also present) | triggers write of policy.toml allowing `google_web_search` + `web_fetch` and passes `--policy <tmpfile>` |
| `WebFetch` | passed verbatim to `--allowedTools` | same single `web_search` tool (codex has no separate fetch — the hosted `web_search` tool covers "fetch this URL" semantics) | same — a single policy file handles both; no duplicated flags |

**PermissionMode translation:**
- claude-code: `--permission-mode <mode>` (v1 contract, unchanged).
- codex: PermissionMode `bypassPermissions` → no separate flag needed;
  codex's tool-call auto-approval is driven by `--sandbox read-only`
  (already in our invocation) + `--ephemeral`. PermissionMode is
  recorded in the executor for interface compat but does not change
  argv.
- gemini-cli: PermissionMode `bypassPermissions` → write the
  embedded policy TOML to `$GEMINI_CLI_HOME/policy.toml` and append
  `--policy $GEMINI_CLI_HOME/policy.toml` to argv. Empty
  PermissionMode → no policy file written, no `--policy` flag;
  gemini's default-deny on `web_fetch` in headless applies (which
  is exactly what ballot subprocesses want). Chosen over `--yolo`
  because `--yolo` emits a deprecation warning in gemini 0.38.2
  and widens the allow-list to `*`.

**Empty `AllowedTools` is the ballot path.** Webfetch hardcodes
`AllowedTools: nil` on ballot spawns (ADR-0010 D17 + F17). Codex
and gemini executors check the slice length: empty → no translation
flags appended. This is mechanism, not backward-compat surface —
there is one user (the author) and no v1/v2 profiles in flight.

## Per-executor invocation specs

### Codex

```go
argv := []string{
    c.binary(), "exec",
    "-m", req.Model,
    "--sandbox", "read-only",
    "--skip-git-repo-check",
    "--ephemeral",
    "--color", "never",
    "-",  // stdin prompt
}
// Translation: any web tool in AllowedTools → enable web_search once.
if containsWebTool(req.AllowedTools) {
    argv = append(argv[:len(argv)-1],
        "-c", "tools.web_search=true",
        "-")
}
```

- **MapModel:** identity.
- **Env:** inherit `os.Environ()`. Codex reads auth from
  `~/.codex/auth.json` or env (OpenAI API key).
- **BinaryName:** `"codex"`.
- **Rate-limit markers** (source: `codex-rs/protocol/src/error.rs`;
  substring match only — no regex fallback per strict-YAGNI; a
  future un-matched 429 is a one-line PR to extend this list):
  1. `you've hit your usage limit`
  2. `quota exceeded. check your plan`
  3. `selected model is at capacity`
  4. `exceeded retry limit, last status: 429`
  5. `upgrade to plus to continue`
- **HelpCmd:** `codex /status`.

### Gemini

```go
argv := []string{
    g.binary(),
    "-m", req.Model,
    "-o", "text",
    // stdin carries prompt (no -p)
}
// Translation: when PermissionMode == bypassPermissions (the
// webfetch expert-path signal), write embedded policy TOML to
// $GEMINI_CLI_HOME/policy.toml and pass --policy. Empty →
// nothing added; gemini's default-deny on web_fetch in headless
// applies (ballot invariant).
if req.PermissionMode == "bypassPermissions" {
    policyPath := filepath.Join(tmpHome, "policy.toml")
    if err := os.WriteFile(policyPath, []byte(geminiPolicyTOML), 0o600); err != nil {
        return executor.Response{}, fmt.Errorf("gemini: write policy: %w", err)
    }
    argv = append(argv, "--policy", policyPath)
}
```

Embedded policy body (`//go:embed policy.toml` or as a string
constant):

```toml
[[rule]]
toolName = ["google_web_search", "web_fetch"]
decision = "allow"
priority = 100
```

- **MapModel:** identity.
- **Env:** inherit (plus `GEMINI_CLI_HOME=<ephemeral tmp dir>` set
  per-call by the executor for no-session-persistence parity). No
  API-key env var — auth is OAuth against the user's Google
  subscription (set up once via gemini's login flow; credentials
  cached under `~/.gemini/`). Known upstream bug #22648: OAuth-
  personal can infinite-loop on 429 — mitigated by our per-expert
  timeout + runner SIGTERM/SIGKILL.
- **BinaryName:** `"gemini"`.
- **Rate-limit markers** (source:
  `packages/core/src/utils/googleQuotaErrors.ts`):
  1. `RESOURCE_EXHAUSTED`
  2. `QUOTA_EXHAUSTED`
  3. `RATE_LIMIT_EXCEEDED`
  4. `exceeded your current quota`
  5. `Please retry in`
- **HelpCmd:** `check https://aistudio.google.com/apikey for quota and billing`.

## Alternatives considered

**a) One multi-CLI dispatch executor.** Rejected — couples every new
CLI to a shared file; violates ADR-0005's extension model.

**b) Hot-spare / fallback mode.** Rejected — goal is diversity *in*
the debate, not resilience.

**c) Opt-in multi-CLI profile, keep claude as default.** Rejected —
the entire point is making cross-vendor the *default*; operators can
still write single-CLI profiles.

**d) Semantic tier aliases (`model: smart`).** Rejected — stale fast,
meaning drifts per vendor; literal IDs keep model choice in YAML
where it belongs.

**e) L1 flag-based tool lockdown.** N/A after webfetch — webfetch's
whole point is experts SHOULD use tools. Irrelevant alternative.

**f) Ship priors-only codex/gemini in v3; translate in v4.**
Rejected per user requirement: webfetch quality must hold across all
three vendors in the v3 landing. Priors-only codex/gemini would
regress the cross-vendor peer signal exactly on research-heavy
questions.

**g) Use `--yolo` for gemini (broad approval bypass).** Rejected —
`--yolo` allows `*`, emits a stderr warning in 0.38.2, and is
flagged for removal in 1.0. Wrong answer twice over.

**h) Use `--allowed-tools google_web_search,web_fetch --yolo` for
gemini.** Rejected — `--allowed-tools` and `--yolo` are both
deprecated-with-removal-pending in 0.38.2. Emits deprecation
warnings on every call. Policy Engine (`--policy <file>`) is the
forward-compatible surface per
`gemini.com/docs/core/policy-engine`.

**h') Policy Engine via `--policy <file>`.** Chosen. Executor
writes a 4-line TOML (`[[rule]] toolName=["google_web_search",
"web_fetch"] decision="allow" priority=100`) to
`$GEMINI_CLI_HOME/policy.toml` per call (the ephemeral home
already exists for session-persistence). Passes `--policy <that
path>` in argv. No deprecation warnings. No `--yolo`. Clean stdout.
Smoke-verified 2026-04-25.

**i) Dual-enable codex: `-c tools.web_search=true` AND attempt to
enable a separate fetch tool.** Rejected — codex doesn't have a
separate fetch. The hosted `web_search` tool's model can fetch URLs
when prompted. The claude `WebFetch` call maps to the same codex
`web_search` tool — no duplication, no missing functionality.

## Research backing

1. **Zhang et al. 2025** (arXiv:2502.08788) — MAD beats single-agent
   baselines *primarily* under heterogeneity. Direct justification
   for cross-vendor.
2. **Liu et al. 2025 "Groupthink in LLM Ensembles"** (TO-VERIFY
   citation) — same-family models show correlated errors; Condorcet
   independence violated.
3. **A-HMAD (Zhou & Chen 2025)** — role diversity helps; v3 does NOT
   ship role-diversity (same prompts across experts), only vendor-
   diversity.
4. **Wan et al. 2025 "Talk Isn't Always Cheap"** (arXiv:2509.05396)
   — sycophancy in homogeneous debate. Cross-vendor peers reduce it.

Honest gap: no published head-to-head compares three-vendor debate
with matched prompts against three-persona-single-vendor debate. We
infer from aggregate findings.

## Risks

**R-v3-01: Per-CLI auth friction.** Mitigation: `council init` live-
probe fails fast per CLI with actionable message.

**R-v3-02: Headless gotchas differ.** Mitigation: invocation specs
codify working flags; live tests catch regressions.

**R-v3-03: Tool-surface per vendor requires empirical verification
on every CLI release.** Codex's `-c tools.web_search=true` and
gemini's `--policy <file>` are current-gen (smoke-verified
2026-04-25 against codex-cli 0.124.0 + gemini-cli 0.38.2). Either
may shift. Mitigation: translation code lives in one place per
executor; a CLI update that breaks it is a one-line PR to that
executor (plus the live smoke failing to flag it).

**R-v3-04: Rate-limit pattern drift.** Mitigation: marker lists
additive + case-insensitive + regex fallback.

**R-v3-05: Cold-start latency on Node-based CLIs.** Webfetch already
bumps timeout to 300s; comfortable margin.

**R-v3-06: Model-ID rotation.** Mitigation: literal IDs; YAML edit,
not code change; `council init --force` regenerates.

**R-v3-07: Single-vendor outage during `council init`.** Mitigation:
logged verdict + skip; `--force` re-runs after recovery.

**R-v3-08: Gemini Policy Engine TOML schema drift.** The
`[[rule]] toolName=... decision="allow" priority=N` shape is
current-gen. Gemini-cli's `docs/core/policy-engine` indicates the
schema is stable enough to be the replacement for deprecated
`--allowed-tools`, but it may evolve before 1.0 GA. Mitigation:
policy body is 4 lines embedded in the executor; a schema change
is a one-line edit to that string.

**R-v3-09: Codex's hosted `web_search` tool pricing / availability.**
OpenAI may change availability of the hosted tool, gate it by tier,
or change pricing. Mitigation: if the flag is set but the tool is
unavailable for the account, codex either errors or silently ignores
— the live verification plan detects this.

## Fitness functions

Append to v2.md §8:

| # | Fitness | Concrete check |
|---|---|---|
| F18 | Codex executor registers | Side-effect import → `executor.Get("codex")` succeeds |
| F19 | Gemini executor registers | Side-effect import → `executor.Get("gemini-cli")` succeeds |
| F20 | BinaryName() returns expected | claude-code→"claude"; codex→"codex"; gemini-cli→"gemini" |
| F21 | Preflight rejects missing binary | Profile referencing missing binary → preflight error names binary + profile line |
| F22 | `council init` idempotent | Two runs without `--force` → second exits 0 without overwriting |
| F23 | `council init --force` regenerates | Second run with `--force` overwrites |
| F24 | Live-probe gate | Mock CLI failing probe excluded from generated profile |
| F25 | Literal model IDs pass through | `model: gpt-5.5` → `codex exec -m gpt-5.5 ...` (identity MapModel) |
| F26 | Codex enables web_search when AllowedTools contains web | `AllowedTools=["WebSearch"]` → argv contains `-c tools.web_search=true`. `AllowedTools=nil` → NO such flag |
| F27 | Gemini writes policy + emits --policy when PermissionMode is bypassPermissions | `PermissionMode="bypassPermissions"` → `$GEMINI_CLI_HOME/policy.toml` exists with the allow-rule body; argv contains `--policy <that path>`. Empty PermissionMode → NO policy file, NO `--policy` flag; argv does NOT contain `--allowed-tools` or `--yolo` (both deprecated) |
| F28 | Claude routing unchanged | Existing webfetch F13/F14 still pass for claudecode executor |
| F29 | Live codex smoke — fetches a URL | `COUNCIL_LIVE_CODEX=1` gated test: prompt "cite current Go version with URL", assert stdout contains `https://` |
| F30 | Live gemini smoke — fetches a URL | `COUNCIL_LIVE_GEMINI=1` gated test: same question, assert stdout contains `https://` |

## Status

Proposed. Paired with ADR-0013.

