# Changelog

All notable changes to this project are recorded here.

## Unreleased

### Added

- Experts always spawn with `WebSearch` and `WebFetch` available in R1 and R2 ([ADR-0010](docs/adr/0010-web-tools-for-experts.md)). Policy lives in `pkg/debate/rounds.go` (package-level `defaultExpertTools` / `defaultPermissionMode`); mechanism lives in `pkg/executor/claudecode` (emits `--allowedTools` / `--permission-mode` when the `Request` fields are non-empty). No profile knob, CLI flag, or environment variable.
- `executor.Request` gains `AllowedTools []string` and `PermissionMode string`. Empty values preserve v1 argv shape; the mock executor records both per-Execute for test assertions.
- R1 and R2 shipped prompts augmented in place: `defaults/prompts/independent.md` adds research + citation discipline; `defaults/prompts/peer-aware.md` adds verification discipline + sycophancy resistance ("prior-round consensus is NOT ground truth").
- Smoke suite: F13 asserts every expert spawn carries `["WebSearch","WebFetch"]` + `bypassPermissions`; F17 asserts every ballot spawn is tools-off. `test/smoke/run-web-tools.sh` gates a real WebFetch-using debate behind `COUNCIL_LIVE_CLAUDE=1`.

### Changed

- Per-expert `timeout` in `defaults/default.yaml` bumped from `180s` to `300s` to accommodate tool-using experts.
- Every structural fence in the prompt protocol now carries the session nonce ([ADR-0011](docs/adr/0011-amend-0008-nonce-every-fence.md)). `USER QUESTION` and `CANDIDATES` fences in expert and ballot prompts are `=== … [nonce-<16hex>] ===`.
- Forgery regex in `pkg/prompt/injection.go` tightened to `(?m)^=== .*\[nonce-[0-9a-f]{16}\] ===[ \t\r]*$`. Benign markdown dividers (`=== Table of Contents ===`, `=== Further Reading ===`) in fetched content now pass the scan; only nonce-shaped fences are rejected. `ScanQuestionForInjection` uses the same regex.
- Ballot subprocesses hardcoded to `AllowedTools: nil` in `pkg/debate/vote.go` — independent of the expert-spawn defaults, not "the negation of" them.

### Docs

- New supplement: [`docs/design/v2-web-tools.md`](docs/design/v2-web-tools.md) — one-narrative reference for the web-tools feature.
- `docs/design/v1.md` §7 Request schema updated to list the two new fields with the "empty = v1 behaviour" guarantee.
- README gains a Web tools section covering the hardcoded policy, the 8–15× token-cost envelope, the 3–5 min per-session latency envelope, and the grep-based audit recipe.
