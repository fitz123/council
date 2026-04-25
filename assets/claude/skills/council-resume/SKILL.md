---
description: Resume an interrupted council session — finish without re-running completed stages
argument-hint: 'optional session ID (defaults to newest incomplete)'
allowed-tools: [Bash, Read, AskUserQuestion, TaskOutput, Glob]
---

# council-resume — Continue an Interrupted Session

**SCOPE**: This command ONLY launches `council resume`, monitors progress, and
reports status. It does NOT modify any source code.

`council resume` is idempotent on per-stage `.done` markers — already-finished
experts and ballots are reused rather than respawned, so there's no penalty
for resuming.

## Step 0: Verify CLI Installation

```bash
which council
```

**If not found**, guide installation as in `/council`. Do not proceed until
`which council` succeeds.

## Step 1: Pick a Session

If `$ARGUMENTS` is non-empty, treat it as the session ID and pass it via
`--session <id>` in Step 2 — `council resume` does **not** accept a positional
session argument; the only positional/flag form it supports is `--session`.

Otherwise, list candidate session directories. Guard the listing so a fresh
repo (no `.council/sessions/` yet) reports cleanly instead of erroring:

```bash
test -d .council/sessions && ls -1t .council/sessions/ || echo "no sessions yet"
```

Use Glob `.council/sessions/*/` to enumerate candidates, then apply council's
own resumable predicate (D14, see `pkg/session/resume.go`) so you don't offer
sessions `--session <id>` will reject:

- skip if the session has a top-level `.done` marker,
- skip if `verdict.json` has a final `status` (`ok`, `no_consensus`,
  `quorum_failed_round_1`, `quorum_failed_round_2`,
  `injection_suspected_in_question`, `config_error`, `error`),
- skip if no stage `.done` exists anywhere under `rounds/` (nothing has
  progressed yet — council requires at least one stage to be cached before
  resume is meaningful),
- otherwise: resumable candidate.

Show up to 4 most recent resumable candidates and ask the user via
AskUserQuestion which to resume — or "Newest" to let council pick.

If there are no resumable sessions, council will exit with code 1 ("no
resumable session"). Report that and stop.

## Step 2: Launch council resume in Background

Always pass `-v`. Always strip `CLAUDECODE` and `CLAUDE_CODE_ENTRYPOINT` so
the spawned `claude -p` subprocess does not trip the nested-CLI guard. If the
user supplied a session ID in `$ARGUMENTS`, pass it via `--session <id>`
(never positionally — council will reject):

```bash
mkdir -p .council
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council resume -v \
  [--session <id>] \
  >.council/last-stdout.txt 2>.council/last-stderr.log
```

Run via the Bash tool with `run_in_background: true`. **Save the task_id from
the response** — you need it for the status check.

## Step 3: Confirm Launch

1. Wait 10–15 seconds for resume to settle.
2. Read the first 5 lines of `.council/last-stderr.log` to see which session
   council picked up and which stages it's reusing from cache.
3. Report:

```
council resume started. Task ID: <task_id>

Session: <session-id>
Resuming stages: <summary from stderr — e.g. "round 2 expert A reused from cache, round 2 expert B fresh">
Verbose log: .council/last-stderr.log

Manual monitoring:
  tail -f .council/last-stderr.log
```

**STOP HERE. Do not continue monitoring automatically.**

## Step 4: Progress Check (only on explicit user request)

Same protocol as `/council`:

1. TaskOutput with `block: false`.
2. Read the last 40 lines of `.council/last-stderr.log`.
3. Report current phase, or final status when the process has exited.

Exit codes are identical to `/council`:

| Code | Meaning |
|------|---------|
| 0    | Winner printed to `.council/last-stdout.txt`. |
| 1    | Config error or no resumable session. |
| 2    | Quorum unmet, or ballots tied (no consensus). |
| 6    | Rate-limit quorum failure (per-CLI footer in stderr). |
| 130  | Interrupted again — re-run `/council-resume`. |

**After reporting status, STOP. Do not offer to do anything else.**

## Constraints

- This command is ONLY for resuming and monitoring an interrupted council run.
- Do NOT offer to help with code or run a fresh debate.
- After launch confirmation: wait for the user to explicitly request status.
- After status check: report and stop.
