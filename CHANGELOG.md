# Changelog

All notable changes to this project are recorded here.

## Unreleased

### Added

- `--verbose` now streams the live debate to stderr — per-stage timing line + artifact block (`=== round N expert X (name) ===` and `=== ballot X (name) ===`) as each subprocess finishes, instead of dumping at the end. Closing block carries the verdict header (`=== verdict (winner: X — name, A/N votes) ===`). New typed `debate.Reporter` interface in `pkg/debate/reporter.go`; concrete `stderrReporter` in `cmd/council/reporter.go`. Untrusted artifact bodies are scrubbed of C0/DEL/C1 control characters before stderr to prevent ANSI/OSC terminal-state injection.
- Voters always emit 1–3 sentences of reasoning above the final flush-left `VOTE: <letter>` line. Reasoning persists in `voting/votes/<label>.txt`; `verdict.json` schema is unchanged. The relaxed prompt explicitly forbids whitespace around the VOTE line and standalone `VOTE: <letter>` lines inside reasoning so the parser stays strict.
- `council resume` accepts `-v` / `--verbose` for the same live stream as a fresh run.
- Experts always spawn with `WebSearch` and `WebFetch` available in R1 and R2 ([ADR-0010](docs/adr/0010-web-tools-for-experts.md)). Policy lives in `pkg/debate/rounds.go` (package-level `defaultExpertTools` / `defaultPermissionMode`); mechanism lives in `pkg/executor/claudecode` (emits `--allowedTools` / `--permission-mode` when the `Request` fields are non-empty). No profile knob, CLI flag, or environment variable.
- `executor.Request` gains `AllowedTools []string` and `PermissionMode string`. Empty values preserve v1 argv shape; the mock executor records both per-Execute for test assertions.
- R1 and R2 shipped prompts augmented in place: `defaults/prompts/independent.md` adds research + citation discipline; `defaults/prompts/peer-aware.md` adds verification discipline + sycophancy resistance ("prior-round consensus is NOT ground truth").
- Smoke suite: F13 asserts every expert spawn carries `["WebSearch","WebFetch"]` + `bypassPermissions`; F17 asserts every ballot spawn is tools-off. `test/smoke/run-web-tools.sh` gates a real WebFetch-using debate behind `COUNCIL_LIVE_CLAUDE=1`.

### Changed

- **Breaking (internal):** `pkg/orchestrator.Run` now takes a `debate.Reporter` as its 5th argument. Pass `debate.NopReporter{}` for the previous "silent during run" behavior.
- `--verbose` no longer post-processes the verdict for the timing summary; the per-stage lines stream live as each stage finishes. The closing footer (`voting: winner X (A/N votes)`, session status + folder, verdict block) is unchanged in shape.
- Per-expert `timeout` in `defaults/default.yaml` bumped from `180s` to `300s` to accommodate tool-using experts.
- Every structural fence in the prompt protocol now carries the session nonce ([ADR-0011](docs/adr/0011-amend-0008-nonce-every-fence.md)). `USER QUESTION` and `CANDIDATES` fences in expert and ballot prompts are `=== … [nonce-<16hex>] ===`.
- Forgery regex in `pkg/prompt/injection.go` tightened to `(?m)^=== .*\[nonce-[0-9a-f]{16}\] ===[ \t\r]*$`. Benign markdown dividers (`=== Table of Contents ===`, `=== Further Reading ===`) in fetched content now pass the scan; only nonce-shaped fences are rejected. `ScanQuestionForInjection` uses the same regex.
- Ballot subprocesses hardcoded to `AllowedTools: nil` in `pkg/debate/vote.go` — independent of the expert-spawn defaults, not "the negation of" them.

### Docs

- New supplement: [`docs/design/v2-web-tools.md`](docs/design/v2-web-tools.md) — one-narrative reference for the web-tools feature.
- `docs/design/v1.md` §7 Request schema updated to list the two new fields with the "empty = v1 behaviour" guarantee.
- README gains a Web tools section covering the hardcoded policy, the 8–15× token-cost envelope, the 3–5 min per-session latency envelope, and the grep-based audit recipe.
