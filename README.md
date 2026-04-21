# council

Multi-expert CLI committee. Fan out one question to N expert CLI-instances, a judge synthesizes one final answer. Every run is archived on disk as file artifacts for audit.

**Status:** v1 MVP — single-pass fan-out with a two-persona default profile.

## Why

A single opinion from a single LLM is noisy. Running the same question through multiple expert personas and synthesizing the results is the ralphex pattern applied to general-purpose questions, not just code reviews.

Inspired by:
- [umputun/ralphex](https://github.com/umputun/ralphex) — deterministic Go orchestrator, multi-agent review pipeline.
- [DenisSergeevitch/repo-task-proof-loop](https://github.com/DenisSergeevitch/repo-task-proof-loop) — durable task-folder pattern, evidence-as-artifacts.
- Multi-LLM council pattern — fan-out to N experts + judge synthesizes final answer.

## Install

Requires Go 1.25+ (as declared in `go.mod`) and a working `claude` CLI on `$PATH` (Claude Code subscription).

```
go install github.com/fitz123/council/cmd/council@latest
```

Or build from a clone:

```
git clone https://github.com/fitz123/council.git
cd council
go build -o council ./cmd/council
```

The result is a single binary with no runtime dependencies beyond `claude`.

## Usage

Ask a question directly:

```
council "design a basic auth system for a small SaaS"
```

Pipe a long question via stdin (use `-` as the positional argument):

```
cat question.md | council -
```

Both forms run the question through every expert in the active profile in parallel, then a judge synthesizes a single final answer. The answer is printed to stdout; transcripts and artifacts land in `./.council/sessions/<id>/`.

## Default profile

Ships with two expert personas plus a judge, all served by the `claude-code` executor:

- `independent` — direct substantive answer (sonnet).
- `critic` — surface counter-arguments, hidden assumptions, failure modes (sonnet).
- `judge` — synthesizes the surviving expert outputs into one answer (opus).

Quorum defaults to `1` — a single expert surviving is enough for the judge to produce an answer. See [ADR-0005](docs/adr/0005-single-cli-multiple-personas.md) for why MVP ships two personas through one CLI rather than one persona or many CLIs.

## Config

Profiles are loaded from the first of:

1. `./.council/default.yaml` (cwd-local, highest priority).
2. `~/.config/council/default.yaml` (user-global).
3. Embedded defaults compiled into the binary (used when neither file exists; the binary does not materialize a copy on first run).

Schema, field semantics, and the canonical example live in [`docs/design/v1.md` §5](docs/design/v1.md). The validator strictly rejects unknown keys — anticipated v2 fields (`effort`, `notify_script`, prompt-file frontmatter) intentionally fail in v1.

`prompt_file` values are resolved relative to the config file's directory. If you write your own `./.council/default.yaml`, put the prompt markdown alongside it (e.g. `./.council/prompts/judge.md`) — the embedded defaults under [`defaults/`](defaults/) are a ready-made starting template.

The `-p`/`--profile` flag is reserved for v2 multi-profile support; v1 accepts only `-p default` (its default value) and returns a config error for any other name. Running `council "..."` without `-p` is the intended form.

### Environment

council injects `CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000` into each `claude` subprocess so the judge has room to synthesise long answers. This overrides any value exported in your shell for child invocations only.

## Verbose mode

`-v` streams structured progress to stderr while the answer still goes to stdout:

```
$ council -v "what is 2+2?"
[17:02:14] council v0.1.0 — session 2026-04-19T17-02-14Z-fizzy-jingling-quokka
[17:02:14] profile: default (2 experts, quorum 1) from embedded
[17:02:14] spawning expert: independent (claude-code, sonnet)
[17:02:14] spawning expert: critic (claude-code, sonnet)
[17:02:14] spawning judge (claude-code, opus)
[17:02:47] expert independent: ok in 17.3s (retries=0)
[17:02:47] expert critic: ok in 19.1s (retries=0)
[17:02:47] judge: done in 14.1s (retries=0)
[17:02:47] session ok: 33.4s total
[17:02:47] session folder: ./.council/sessions/2026-04-19T17-02-14Z-fizzy-jingling-quokka
```

The `profile: … from <source>` line names which config file the profile loaded from (cwd-local path, user-global path, or `embedded`). Per-role completion lines carry the verdict status (`ok` / `failed` / `interrupted`) and retry count so the stderr stream and `verdict.json` agree on what happened.

Transcripts always land in the session folder, regardless of `-v`.

## Exit codes

| Code | Meaning                                                            |
|------|--------------------------------------------------------------------|
| 0    | Success — answer printed to stdout, `verdict.json` written.        |
| 1    | Config or validation error (bad YAML, unknown field, missing file).|
| 2    | Quorum not met — too many experts failed.                          |
| 3    | Judge failed after retry.                                          |
| 130  | Interrupted by SIGINT/SIGTERM. Partial `verdict.json` is written.  |

## Deployment constraint — do not nest

`council` spawns `claude -p` subprocesses. The Claude Code CLI forbids nested invocation: running `claude` from inside an active Claude Code session loses output and may crash the parent.

- **Safe to run from:** a fresh shell, a cron entry, a launchd job, a script that is **not** itself a Claude Code session.
- **Unsafe:** `council` invoked from inside a running `claude` session's Bash tool.

This is a property of the underlying CLI, not council itself. See [`docs/design/v1.md` §12](docs/design/v1.md).

## Maintenance

Session folders accumulate under `./.council/sessions/`. Prune them manually for now:

```
find .council/sessions -mindepth 1 -maxdepth 1 -type d -mtime +30 -exec rm -rf {} +
```

A `council gc` subcommand is on the v2 roadmap.

## Design

- [`docs/design/v1.md`](docs/design/v1.md) — MVP v1 spec.
- [`docs/design/v2.md`](docs/design/v2.md) — debate-rounds extension (in design).
- [`docs/adr/`](docs/adr/) — architectural decision records.
- [`docs/architect-review.md`](docs/architect-review.md) — systems-architect methodology review of the spec.

## License

MIT. See [LICENSE](LICENSE).
