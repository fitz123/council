---
description: Resume an interrupted council session — finish without re-running completed stages
argument-hint: 'optional session ID (defaults to newest incomplete)'
allowed-tools: [Bash, Read, AskUserQuestion, TaskOutput, Glob]
---

# council-resume — Continue an Interrupted Session

**SCOPE**: Launch `council resume`, monitor, report. Do nothing else.

`council resume` is idempotent on per-stage `.done` markers — finished
experts and ballots are reused, not respawned.

## Step 0: Verify CLI

```bash
which council
```

If missing: see `/council` Step 0. Do not proceed until `which council`
succeeds.

## Step 1: Pick a Session

If `$ARGUMENTS` is non-empty, treat it as the session ID — pass it via
`--session "$id"` (council resume rejects positional session args).

Otherwise list candidates without erroring on a fresh repo:

```bash
test -d .council/sessions && ls -1t .council/sessions/ || echo "no sessions yet"
```

A session is **resumable** (D14 predicate, `pkg/session/resume.go`) iff:

- no top-level `.done` marker, AND
- `verdict.json.status` is not one of `ok`, `no_consensus`,
  `quorum_failed_round_1`, `quorum_failed_round_2`,
  `injection_suspected_in_question`, `config_error`, `error`, AND
- at least one stage `.done` exists somewhere under `rounds/`.

Show up to 4 most recent resumable sessions via AskUserQuestion (plus
"Newest" → let council pick). If none, council exits 1 (`no resumable
session`) — report and stop.

## Step 2: Launch (background)

```bash
errlog=$(mktemp /tmp/council-resume-stderr.XXXXXXXX)
echo "$errlog"
```

Pick **one** of the two forms below (never use `[…]` placeholder syntax in
the bash you actually run):

**A — newest resumable** (no `$ARGUMENTS`):

```bash
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council resume -v \
  > /dev/null 2> "$errlog"
```

**B — explicit session ID**:

```bash
SESSION="<the-validated-session-id>"
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council resume -v --session "$SESSION" \
  > /dev/null 2> "$errlog"
```

Run with `run_in_background: true`. **Save the `task_id` and `$errlog`.**

## Step 3: Confirm Launch

After 10–15s, read first 5 lines of `$errlog` → which session and which
stages are reused from cache. Report:

```
council resume started. Task ID: <task_id>
Session: <session-id>
Resuming: <e.g. "round 2 expert A reused, expert B fresh">
Session folder: ./.council/sessions/<session-id>/
Verbose log: <errlog>

Manual: tail -f <errlog>
```

**STOP.** Do not poll automatically.

## Step 4: Status (on explicit user request)

Same protocol as `/council` Step 4 — `TaskOutput`, tail of `$errlog`,
report phase or final outcome. Exit codes are identical:

| Exit | Action |
|------|--------|
| 0    | Read `./.council/sessions/<id>/output.md` and print it. |
| 1    | Config error or no resumable session. |
| 2    | Quorum unmet, or ballots tied. |
| 6    | Rate-limit quorum failure. |
| 130  | Interrupted again — re-run `/council-resume`. |

Then **STOP**.
