# council Claude Code Plugin

This directory contains the Claude Code plugin configuration for council.

## Files

- `plugin.json` — Plugin manifest with metadata and version.
- `marketplace.json` — Marketplace catalog for single-plugin distribution.

## Installation

Users can install via the plugin marketplace:

```bash
/plugin marketplace add fitz123/council
/plugin install council@fitz123-council
```

## Marketplace Structure

This repository serves as both:

1. The council CLI tool source code.
2. A single-plugin Claude Code marketplace.

The marketplace references `./` as the plugin source. Plugin skills live in
`assets/claude/skills/`, keeping all Claude Code related files organized
together.

## Skills

| Skill              | Purpose                                              |
|--------------------|------------------------------------------------------|
| `/council`         | Ask the council a question and monitor progress.     |
| `/council-init`    | Probe installed CLIs and write the per-host profile. |
| `/council-resume`  | Resume an interrupted session.                       |

## Nested Claude Code sessions

`council` spawns `claude -p`, `codex`, and `gemini` subprocesses. The Claude
Code CLI rejects nested invocation when `CLAUDECODE=1` is set. The skills
launch council with `env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT` so the
spawned `claude -p` subprocess does not see itself as nested. This matches
the workaround ralphex applies inside its Go binary.
