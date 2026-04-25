---
description: Probe installed CLIs and write the per-host council profile
allowed-tools: [Bash, Read, AskUserQuestion]
---

# council-init — Generate the Per-Host Profile

**SCOPE**: Run `council init`, report what it produced. Nothing else.

## Step 0: Verify CLI

```bash
which council
```

If missing: `go install github.com/fitz123/council/cmd/council@latest`.

## Step 1: Probe both profile locations

`council init` only writes `~/.config/council/default.yaml`, but `council`
itself resolves profiles in this order on every run:

1. `./.council/default.yaml` (cwd-local — wins if present)
2. `~/.config/council/default.yaml` (user-global)
3. Embedded defaults compiled into the binary

A cwd-local profile silently shadows whatever init produces. Probe both:

```bash
test -f .council/default.yaml && echo "local:  found .council/default.yaml" || echo "local:  missing .council/default.yaml"
test -f ~/.config/council/default.yaml && echo "global: found ~/.config/council/default.yaml" || echo "global: missing ~/.config/council/default.yaml"
```

If global exists: ask via AskUserQuestion whether to keep or regenerate
(`Keep current` exits, `Regenerate` runs `council init --force`). If a
local profile also exists, warn that it shadows the global one — `/council`
will keep using the local file regardless of what init writes.

## Step 2: Surface vendor CLIs

```bash
for b in claude codex gemini; do
  if command -v "$b" >/dev/null 2>&1; then
    echo "$b: $(command -v "$b")"
  else
    echo "$b: not found"
  fi
done
```

Each is subscription-based — no API keys:

| Executor      | Binary   | Install                                              | Auth                                      |
|---------------|----------|------------------------------------------------------|-------------------------------------------|
| `claude-code` | `claude` | https://docs.claude.com/claude-code                  | `claude /login`                           |
| `codex`       | `codex`  | `brew install codex` (or per-platform release)       | `codex login`                             |
| `gemini-cli`  | `gemini` | `brew install gemini-cli` (or per-platform release)  | run `gemini` once → OAuth browser flow    |

Proceed regardless — init writes a profile with whatever subset is verified.

## Step 3: Run init

Strip `CLAUDECODE` so the live-probe of the `claude-code` executor (which
spawns `claude -p`) does not trip the nested-CLI guard:

```bash
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT council init
```

…or `council init --force` when regenerating. Report verified / skipped
CLIs from stderr.

## Step 4: Confirm

```bash
ls -la ~/.config/council/default.yaml
head -20 ~/.config/council/default.yaml
```

If `./.council/default.yaml` exists, repeat the shadowing warning — `/council`
will keep using the local file until it is removed or edited.

Quorum scales as `min(2, len(verified))`. Zero verified is a hard error.

**STOP.** Do not offer to run a debate.
