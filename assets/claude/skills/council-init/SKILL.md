---
description: Probe installed CLIs and write the per-host council profile
allowed-tools: [Bash, Read, AskUserQuestion]
---

# council-init — Generate the Per-Host Profile

**SCOPE**: This command ONLY runs `council init` and reports what it produced.
It does NOT modify any source code or run a debate.

## Step 0: Verify CLI Installation

```bash
which council
```

**If not found**, guide installation:

- **Any platform with Go**: `go install github.com/fitz123/council/cmd/council@latest`

**Do not proceed until `which council` succeeds.**

## Step 1: Check for Existing Profile

```bash
ls -la ~/.config/council/default.yaml
```

If the file already exists, ask the user how to proceed via AskUserQuestion:

- header: "Profile"
- question: "A profile already exists at `~/.config/council/default.yaml`. Overwrite?"
- options:
  - label: "Keep current"
    description: "Leave the existing profile untouched and exit"
  - label: "Regenerate"
    description: "Overwrite with `council init --force` (re-probes installed CLIs)"

If the user picks "Keep current", report the path and stop.

## Step 2: Probe and Confirm Vendor CLIs

`council init` registers each executor (`claude-code`, `codex`, `gemini-cli`),
runs `exec.LookPath` to confirm the binary, and live-probes ("respond with the
word OK", 30s timeout) to confirm auth.

Quickly surface which CLIs are present so the user knows what to expect:

```bash
for b in claude codex gemini; do
  if command -v "$b" >/dev/null 2>&1; then
    echo "$b: $(command -v "$b")"
  else
    echo "$b: not found"
  fi
done
```

If any are missing, mention them and link to the install instructions in the
README (each is subscription-based — no API keys):

| Executor      | Binary   | Install                                                 | Auth                                  |
|---------------|----------|---------------------------------------------------------|---------------------------------------|
| `claude-code` | `claude` | See https://docs.claude.com/claude-code                 | `claude /login`                       |
| `codex`       | `codex`  | `brew install codex` (or per-platform release)          | `codex login`                         |
| `gemini-cli`  | `gemini` | `brew install gemini-cli` (or per-platform release)     | run `gemini` once → OAuth browser flow |

Proceed regardless — `council init` happily writes a profile with whatever
subset of CLIs is actually verified, and the embedded fallback is claude-only.

## Step 3: Run init

Run synchronously (init is fast — it's just a probe + file write). Strip
`CLAUDECODE` and `CLAUDE_CODE_ENTRYPOINT` so the live-probe of the
`claude-code` executor (which spawns `claude -p`) does not trip the nested-CLI
guard:

```bash
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council init
```

…or, if the user picked "Regenerate" in Step 1:

```bash
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council init --force
```

Report any verified / skipped CLIs from the command's stderr output. Skipped
CLIs are reported with a reason (binary not on PATH, auth probe failed, etc.).

## Step 4: Confirm

Show the resulting profile path and quorum sizing:

```bash
ls -la ~/.config/council/default.yaml
head -20 ~/.config/council/default.yaml
```

Quorum scales as `min(2, len(verified))`. Zero verified CLIs is a hard error —
council exits non-zero with an "install at least one of …" message.

After reporting, STOP. Do not offer to run a debate; the user can invoke
`/council` next if they want.
