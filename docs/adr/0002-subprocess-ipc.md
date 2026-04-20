# ADR-0002: Subprocess IPC via stdin / stdout / exit codes

**Status:** Proposed.

## Context

The orchestrator needs a way to communicate with expert and judge processes. The expert/judge processes are external CLIs (`claude -p`, later `codex`, `gemini-cli`).

Alternatives:

- **HTTP or gRPC** — requires running an RPC server inside each expert process. Not how external CLIs work; overkill.
- **Unix sockets / shared memory** — couples orchestrator and expert to a specific IPC library; doesn't match how `claude -p` or similar CLIs operate.
- **Subprocess stdin / stdout / exit codes** — POSIX-native, zero dependency, matches the shape of the target CLIs.

## Decision

The orchestrator communicates with experts and the judge via:

- **stdin** — full prompt sent as bytes. No argv — argv has an OS-level length limit (`ARG_MAX`, ~256 KB on macOS, ~2 MB on Linux) and a user's question or assembled judge prompt can easily exceed it.
- **stdout** — full answer captured as file content.
- **stderr** — log / diagnostic text. Persisted to a session file only on non-zero exit.
- **exit code** — success / failure signal.

No custom protocol on top.

## Consequences

- **(+)** Zero network dependency; no ports, no daemon.
- **(+)** Replacing `claude -p` with another CLI is a file swap behind the `Executor` interface; the contract is "stdin in, stdout out, exit code".
- **(−)** No streaming partial output with custom semantics — stdout is buffered into a file and consumed after completion. Acceptable for MVP (synthesis happens after all experts finish anyway).
- **(−)** Semantic coupling to the exact flag surface of each CLI. The `claude -p` flags (`--model`, `--output-format`) must be verified against the current release before implementation starts.
