
# council v3 — Multi-CLI Supplement

> Layers on v2.md (as amended by ADR-0010/0011/v2-web-tools.md).
> Authoritative decisions in ADR-0012 + ADR-0013.

## 1. What changes in v3

### 1.1 Executor surface

`pkg/executor/codex/` and `pkg/executor/gemini/`, mirroring
claudecode's structure. `Executor` gains `BinaryName() string`.

Every executor translates webfetch's `AllowedTools` /
`PermissionMode` to its native flag surface. Claude unchanged;
codex adds `-c tools.web_search=true` when web tools requested;
gemini writes a Policy Engine TOML to a per-call ephemeral tmp
dir and passes `--policy <file>` (forward-compatible replacement
for deprecated `--allowed-tools`/`--yolo`). The executor does NOT
set `GEMINI_CLI_HOME` — gemini-cli treats it as the parent of
`.gemini/` (where OAuth creds live), so redirecting it to a fresh
dir would mask the user's `~/.gemini/oauth_creds.json`.

### 1.2 Default profile (written by `council init`)

Embedded `defaults/default.yaml` stays claude-only (safe fallback on
fresh install with only `claude` on PATH). `council init` writes
per-host `~/.config/council/default.yaml`. Generated when all three
CLIs verify:

```yaml
version: 2
name: default
experts:
  - name: claude_expert
    executor: claude-code
    model: opus
    prompt_file: prompts/independent.md
    timeout: 300s
  - name: codex_expert
    executor: codex
    model: gpt-5.5
    prompt_file: prompts/independent.md
    timeout: 300s
  - name: gemini_expert
    executor: gemini-cli
    model: gemini-3.1-pro-preview
    prompt_file: prompts/independent.md
    timeout: 300s
quorum: 2
max_retries: 1
rounds: 2
round_2_prompt_file: prompts/peer-aware.md
voting:
  ballot_prompt_file: prompts/ballot.md
  timeout: 300s
```

- `timeout: 300s` matches webfetch default (tool latency).
- `quorum: 2` — cross-vendor diversity makes one-vendor outage
  common; two-vendor outage rare.
- Subset profiles: `quorum = min(2, len(experts))`.

### 1.3 `council init`

```
council init [--force]
```

1. Idempotent — refuses existing file without `--force`.
2. Detect — `exec.LookPath(executor.BinaryName())` per registered
   executor.
3. Probe — 30s live prompt "respond with OK"; stdout must contain
   "OK".
4. Fail-open — zero verified → non-zero exit.
5. Write — profile per verified set; `quorum = min(2, len)`.
6. Summary printed to stdout.

### 1.4 Preflight

Every run. `exec.LookPath` per expert's `BinaryName()`. Miss → exit
with clear message. No live probe (too slow per-run).

### 1.5 Rate-limit surface (ADR-0013)

- Runner no longer retries 429.
- `*LimitError{Tool, Pattern, HelpCmd}` surfaces immediately.
- Orchestrator absorbs via quorum.
- verdict.json gains `rate_limits[]` additive field.
- Exit code 6 on quorum fail due to rate-limits.

## 2. Walkthrough

§3.2 Anonymization — unchanged (label assignment independent of
executor).
§3.3 R1 — each expert's subprocess receives same prompt; different
vendor; different model. Tool-surface hardcoded in `rounds.go`
reaches all three executors; each translates to native flags per
ADR-0012.
§3.4 Peer aggregate — unchanged.
§3.5 R2 — cross-vendor peers mean real refinement signal.
§3.6 Ballot — unchanged; tools-off per webfetch.
§3.7 Verdict — `rate_limits` field when any expert rate-limited.

## 3. Per-executor notes

### 3.1 Claude-code (unchanged from webfetch)

`--allowedTools WebSearch,WebFetch --permission-mode bypassPermissions
--no-session-persistence` when AllowedTools non-empty (empty →
none of those flags). Rate-limit markers moved from runner to this
executor (see ADR-0013). Markers: `you've hit your limit`,
`usage limit exceeded`, `anthropic rate limit`. HelpCmd:
`claude /usage`.

### 3.2 Codex

argv assembly:
```
codex exec -m <model> --sandbox read-only --skip-git-repo-check \
  --ephemeral --color never -c tools.web_search=true -
```
(the `-c tools.web_search=true` is emitted when AllowedTools is
non-empty; claude's `["WebSearch","WebFetch"]` both trigger the
single codex `web_search` tool.)

Rate-limit markers + HelpCmd per ADR-0012.

### 3.3 Gemini

argv assembly (PermissionMode == bypassPermissions):
```
gemini -m <model> -o text --policy <ephemeral-tmpdir>/policy.toml
```
(stdin-piped prompt; no `-p`.)

Executor writes a 4-line policy TOML at `<ephemeral-tmpdir>/policy.toml`
(per-call `os.MkdirTemp` + `defer os.RemoveAll`) before invoking
gemini. Policy body (embedded in the executor):

```toml
[[rule]]
toolName = ["google_web_search", "web_fetch"]
decision = "allow"
priority = 100
```

No `--yolo`, no `--allowed-tools` — both deprecated in gemini
0.38.2, slated for removal in 1.0. The Policy Engine surface is
the supported forward-compatible path.

### 3.4 Ballots still tools-off

Webfetch Task 2 hardcodes `AllowedTools: nil, PermissionMode: ""`
on ballot spawns. Codex and gemini executors see the empty values
and emit no translation — no `-c tools.web_search=true`, no policy
file written, no `--policy`. Ballot subprocesses stay priors-only
across all three vendors. Consistent with webfetch's intent.

## 4. Injection surface

Unchanged — codex/gemini outputs flow through same nonce-fencing +
forgery-scan path. Per-vendor stylistic differences contained
within `output.md`.

## 5. Token cost

Webfetch estimates 8–15× claude single-pass under tool use. Multi-
cli runs three experts in parallel (each using tools), so wall-clock
stays similar; total token bill *spreads* across three vendor
accounts (3× headroom before any single-vendor cap).

## 6. What operators should know

- Run `council init` after install; re-run with `--force` after
  adding/removing CLIs.
- Each CLI needs its own subscription-based auth, set up once:
  `claude /login` (Anthropic); `codex login` (OpenAI); gemini's
  OAuth flow (Google). All three cache credentials under
  `~/.claude/` / `~/.codex/` / `~/.gemini/` respectively. No API
  keys — the tooling is subscription-based, not pay-per-token.
- Exit code 6 = rate-limit quorum failure (distinct from 2).
- Model IDs are literal.
- All three vendors now use live web tools; question quality in
  cross-vendor debate does NOT regress from webfetch's claude-only
  quality.

