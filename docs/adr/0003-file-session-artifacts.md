# ADR-0003: File-based session artifacts (folder-as-database)

**Status:** Proposed.

## Context

Every council run should be fully reconstructable for audit, debugging, and post-hoc analysis. We need a storage model.

Alternatives:

- **SQLite** — requires schema, migrations, an extra dependency. Overkill for an append-only per-session record.
- **In-memory + optional dump** — loses the trace on crash; defeats the audit purpose.
- **Folder per session with files by role** — grep-able, cat-able, diff-able, backs up with `tar`, works with any sync tool, zero new concepts.

## Decision

Each council run produces a folder `./.council/sessions/<timestamp>-<adj>-<adj>-<noun>/` (petname suffix via `github.com/dustinkirkland/golang-petname`) with:

- `question.md` — the verbatim user question.
- `profile.snapshot.yaml` — frozen copy of the profile that was used (decouples running sessions from config edits).
- `rounds/<n>/experts/<name>/{prompt.md, output.md, stderr.log, .done}` — per-expert evidence for each round. `stderr.log` is written only on failure. v1 always writes `rounds/1/`; v2 debate fills additional rounds.
- `rounds/<n>/judge/{prompt.md, synthesis.md, stderr.log, .done}` — per-round judge evidence.
- `verdict.json` — machine-readable index of the run; atomic write via `tmp + fsync + rename`.

`verdict.json` is the only machine-readable contract; it carries a `"version": 1` field so later schema changes can be detected.

## Consequences

- **(+)** Zero infrastructure — debuggable with `ls`, `cat`, `jq`, `diff`.
- **(+)** Parallel sessions are isolated by petname suffix; no write contention.
- **(+)** Backups / exports are just `tar` of the session folder.
- **(−)** No built-in querying (e.g. "all failed sessions in the last week"). Intentional: `find` + `jq` covers the known use cases; a dedicated query tool is deferred.
- **(−)** Disk grows without a rotation policy. README will document manual cleanup or user-side cron; built-in `council gc` is deferred to v2.
