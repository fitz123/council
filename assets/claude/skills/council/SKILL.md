---
description: Ask the council a question — fan-out to N expert CLIs, two-round debate, distributed vote
argument-hint: 'optional question text (or omit and you will be asked)'
allowed-tools: [Bash, Read, AskUserQuestion, TaskOutput, Glob]
---

# council — Multi-expert Debate

**SCOPE**: This command ONLY launches `council`, monitors progress, and reports
status. Do NOT take any other actions on the codebase.

## Step 0: Verify CLI Installation

Check if council is on PATH:

```bash
which council
```

**If not found**, guide installation:

- **Any platform with Go**: `go install github.com/fitz123/council/cmd/council@latest`
- **From a clone**: `git clone https://github.com/fitz123/council.git && cd council && go build -o council ./cmd/council`

Use AskUserQuestion to confirm installation method, then guide through it. **Do
not proceed until `which council` succeeds.**

## Step 1: Verify Profile

council looks for a profile in this order:

1. `./.council/default.yaml` (cwd-local)
2. `~/.config/council/default.yaml` (user-global)
3. Embedded defaults compiled into the binary (claude-code only)

If neither file exists, the embedded fallback works but only uses `claude-code`.
For the recommended three-CLI cohort (claude-code + codex + gemini), the user
needs the per-host profile.

```bash
ls .council/default.yaml ~/.config/council/default.yaml
```

If both are missing and the user wants the multi-CLI default, suggest running
`/council-init` first and stop. If the user is fine with the embedded fallback,
proceed.

## Step 2: Resolve the Question

- if `$ARGUMENTS` is non-empty: use it verbatim as the question.
- otherwise: ask via AskUserQuestion or just prompt for free-text input.

For multi-paragraph questions, write them to a temp file and pipe via `-`:

```bash
cat /tmp/council-q.md | env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council -v -
```

## Step 3: Launch council in Background

Build the command. Always pass `-v` so per-stage progress streams to stderr.
Always strip `CLAUDECODE` and `CLAUDE_CODE_ENTRYPOINT` so the spawned
`claude -p` subprocess does not trip the nested-CLI guard.

```bash
mkdir -p .council
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council -v "<question>" \
  >.council/last-stdout.txt 2>.council/last-stderr.log
```

Run via the Bash tool with `run_in_background: true`. **Save the task_id from
the response** — you need it for the status check.

A research-heavy run takes 3–5 minutes wall-clock per the README.

## Step 4: Confirm Launch

1. Wait 10–15 seconds for initialization.
2. Read the first 5 lines of `.council/last-stderr.log` to extract the session
   ID (line 1 contains `... — session <id>`) and the profile summary (line 2).
3. Report:

```
council started. Task ID: <task_id>

Question: <first 100 chars of question>
Session: <session-id>
Profile: <profile summary>
Session folder: ./.council/sessions/<session-id>/
Verbose log: .council/last-stderr.log

Manual monitoring:
  tail -f .council/last-stderr.log     # live debate stream
  tail -50 .council/last-stderr.log    # recent activity

council runs autonomously (3–5 min on research questions). The process
continues if you close this conversation.

Ask "check council" to get a status update.
```

**STOP HERE. Do not continue monitoring automatically.**

## Step 5: Progress Check (only on explicit user request)

If the user explicitly asks "check council", "council status", or
"how is council doing":

1. Use TaskOutput with `block: false` to check process status (use the
   `task_id` from Step 3).
2. Read the last 40 lines of `.council/last-stderr.log`.

**If the process is still running**, infer the current phase from the most
recent stage line:

- `round 1 expert <X> ...` → R1 fan-out (blind round).
- `round 2 expert <X> ...` → R2 fan-out (peer-aware round).
- `ballot <X> voted for <Y> ...` → voting stage.

Show the recent stage lines.

**If the process exited** (TaskOutput shows completion):

- Exit code `0` → success. Print `.council/last-stdout.txt` (winner's R2 body).
  Mention the verdict header from the tail of `last-stderr.log`.
- Exit code `2` → no consensus. Point at `output-<label>.md` files in the
  session folder.
- Exit code `6` → rate-limit quorum failure. Surface the per-CLI footer from
  the stderr log.
- Exit code `1` → config / preflight error. Show the relevant stderr lines.
- Exit code `130` → interrupted. Suggest `/council-resume`.

**After reporting status, STOP. Do not offer to do anything else.**

## Constraints

- This command is ONLY for launching and monitoring council.
- Do NOT offer to help with code, commits, PRs, or anything else.
- Do NOT take actions on the codebase based on the verdict.
- After launch confirmation: wait for the user to explicitly request a status
  check.
- After status check: report and stop.

## Nested Claude Code Sessions

council itself does not strip `CLAUDECODE` from child processes (unlike
ralphex). The skill works around this by invoking council with
`env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT`, so the spawned `claude -p`
subprocess sees a clean environment. Running council from a standalone
terminal is still fine — the env stripping is a no-op when the vars aren't set.
