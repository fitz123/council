---
description: Ask the council a question — fan-out to N expert CLIs, two-round debate, distributed vote
argument-hint: 'optional question text (or omit and you will be asked)'
allowed-tools: [Bash, Read, Write, AskUserQuestion, TaskOutput, Glob]
---

# council — Multi-expert Debate

**SCOPE**: Launch `council`, monitor progress, report status. Do nothing else.

## Step 0: Verify CLI

```bash
which council
```

If missing: `go install github.com/fitz123/council/cmd/council@latest`. Do
not proceed until `which council` succeeds.

## Step 1: Question

- `$ARGUMENTS` if non-empty, else AskUserQuestion / free-text input.
- Write the question to a tempfile via the **Write tool** (never `echo` /
  `printf` / heredoc — question text may contain backticks, `$(…)`, quotes,
  or newlines that the shell would interpret):

```bash
qfile=$(mktemp /tmp/council-question.XXXXXXXX)
echo "$qfile"
```

Then call Write with `file_path=$qfile`, `content=<raw question text>`.

## Step 2: Launch (background)

`-v` is required for monitoring. `env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT`
is required so the spawned `claude -p` does not trip the nested-CLI guard.
Question goes via stdin redirection — never argv:

```bash
errlog=$(mktemp /tmp/council-stderr.XXXXXXXX)
echo "$errlog"
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council -v - \
  < "$qfile" > /dev/null 2> "$errlog"
```

Run with `run_in_background: true`. **Save the `task_id` and `$errlog`** —
both are needed for status checks. Stdout is discarded because council
writes the winner's body to `./.council/sessions/<id>/output.md` on success.

A research-heavy run takes 3–5 min wall-clock.

## Step 3: Confirm Launch

After 10–15s, read first 5 lines of `$errlog` to extract the session ID
(line 1: `... — session <id>`) and profile summary (line 2). Report:

```
council started. Task ID: <task_id>
Question: <first 100 chars>
Session: <session-id>
Session folder: ./.council/sessions/<session-id>/
Verbose log: <errlog>

Manual: tail -f <errlog>
```

**STOP.** Do not poll automatically.

## Step 4: Status (on explicit user request)

Trigger words: "check council", "council status", "how is council doing".

1. `TaskOutput` (block: false) for the saved `task_id`.
2. Read tail of `$errlog`.

Running → report current phase from the most recent stage line:

- `round 1 expert <X> ...` → R1.
- `round 2 expert <X> ...` → R2.
- `ballot <X> voted for <Y> ...` → voting.

Exited:

| Exit | Action |
|------|--------|
| 0    | Read `./.council/sessions/<id>/output.md` and print it. Tail of `$errlog` carries the verdict header. |
| 1    | Config / preflight error — show relevant stderr lines. |
| 2    | No consensus — point at `output-<label>.md` in the session folder. |
| 6    | Rate-limit quorum failure — surface the per-CLI footer from `$errlog`. |
| 130  | Interrupted — suggest `/council-resume`. |

Then **STOP**.

## Nested Claude Code Sessions

council does not strip `CLAUDECODE` from spawned `claude -p` subprocesses
(unlike ralphex). The `env -u` prefix is the workaround. From a standalone
terminal it is a no-op (vars aren't set).
