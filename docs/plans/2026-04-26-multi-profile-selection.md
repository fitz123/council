# Multi-Profile Selection — Implementation Plan

## Overview

Make `-p` / `--profile` actually pick a profile. Today the flag accepts only the literal name `default` and any other value is rejected up-front in `cmd/council/main.go:142`; the v2/v3 multi-profile hook was left dangling. After this change the flag accepts either a **name** (resolved to `<name>.yaml` at the existing precedence locations) or an **explicit path** (when the value contains `/` or ends in `.yaml`). Use case: keep a `cheap.yaml` (haiku / gpt-mini) alongside the default `prod.yaml` (opus / gpt-5.5) and switch with one flag.

**Source of truth:** this plan + `pkg/config/loader.go` precedence comment (lines 88–93). No new ADR — the change is mechanical and stays inside the existing config + CLI surface.

## Context (from discovery)

- `cmd/council/main.go:96–99` — `-p` / `--profile` flag declaration; default `"default"`.
- `cmd/council/main.go:142–145` — equality gate that rejects any non-default name with `not supported (only "default" is available)`.
- `pkg/config/loader.go:88–133` — `Load(cwd)` does the precedence walk:
  1. `<cwd>/.council/default.yaml`
  2. `<home>/.config/council/default.yaml`
  3. embedded fallback (`loadFromEmbedded`).
  Every step hardcodes `default.yaml`.
- `pkg/config/loader.go:25–27` — `ErrNoConfig` message hardcodes `default.yaml`.
- `pkg/config/loader.go:138–141` — `LoadFile(path)` already loads any path; it's the building block we'll reuse for path-mode.
- `cmd/council/init.go` — `council init` writes `~/.config/council/default.yaml`. **Out of scope:** init keeps writing only `default.yaml` for now; users add more profiles by copy + edit.
- `cmd/council/main.go:315` — resume reads `profile.snapshot.yaml` from the session folder, not by name. **Unaffected** by this change.
- `pkg/session` — session folder name does not encode the profile name; the snapshot YAML records `name:`. **Unaffected.**
- README.md:131 — currently states `-p default` only. Needs replacement.

**Files touched by this plan:**

- `pkg/config/loader.go` — add `LoadByName(cwd, name)`; refactor `Load(cwd)` as the `name == "default"` wrapper; add `profileNameRE`; expand `ErrNoConfig` to a per-name `ErrProfileNotFound`.
- `pkg/config/loader_test.go` — new tests covering name precedence, path mode, name validation, missing-non-default-no-embed-fallback, default-still-falls-back.
- `cmd/council/main.go` — drop the equality gate; call `config.LoadByName(cwd, profileName)`; update `--profile` flag help.
- `cmd/council/main_test.go` — replace `TestRun_NonDefaultProfileRejected` with the new test matrix.
- `cmd/council/main.go` (printHelp) — short addition mentioning name + path.
- `README.md` — replace the "v2 accepts only `-p default`" line with a Profiles subsection.
- `CHANGELOG.md` — one bullet under Unreleased.

**Key contracts to preserve:**

- `council "q"` (no flag) keeps current behaviour — defaults to `-p default`, embedded fallback works.
- `council resume` is unchanged (snapshot-based).
- `council init` is unchanged (still writes `default.yaml`).
- The `-p`/`--profile` flag stays string-typed; default value stays `"default"`.
- `pkg/config.Profile` shape is unchanged.

## Development Approach

- **Testing approach: TDD** — tests first, see them fail, implement, see them pass.
- Complete each task fully before moving to the next.
- **Every task includes new/updated tests** for code changes in that task.
- **All tests pass before starting next task.**
- Run `go test ./...` after each task.

## Testing Strategy

- **Unit tests:** `pkg/config/loader_test.go` covers all `LoadByName` paths.
- **CLI tests:** `cmd/council/main_test.go` covers the new `-p` matrix end-to-end through `run()` (no subprocess).
- **No new smoke needed** — the existing F-suite all run with the default profile, which is unchanged behaviourally.
- **Coverage target:** parity with current loader/main coverage (both already at ~90%).

## Design Decisions

- **Path mode trigger:** `strings.ContainsRune(value, '/') || strings.HasSuffix(value, ".yaml")`. A bare `cheap` is a name; `./cheap.yaml`, `/abs/cheap.yaml`, `cheap.yaml` (relative file in cwd) are paths. Rationale: profile names should not collide with these — name validation (below) explicitly forbids `/` and `.` in names.
- **Name validation regex:** `^[a-zA-Z0-9][a-zA-Z0-9_-]*$`. Same shape as `expertNameRE` (`pkg/config/loader.go:23`). Blocks `..`, `/`, hidden-file shapes, spaces. Applied **before** the path-mode check is even evaluated (path mode is detected first; if not path, then name must validate).
- **Embedded fallback only for `default`.** A `-p cheap` with no `cheap.yaml` anywhere errors out (`ErrProfileNotFound`) — silently falling back to embedded `default` would be surprising (the user explicitly named `cheap`). `-p default` with no on-disk file still embed-falls-back to preserve zero-config UX.
- **Path mode has no fallback.** Missing path → error; never silently substitute embedded.
- **`Load(cwd)` stays as a thin wrapper over `LoadByName(cwd, "default")`** so internal callers (none today, but the snapshot loader path imports `loadFromEmbedded`) need no churn.
- **Error messages** include the resolved candidate paths so the user can see exactly what was checked. Mirrors the current `ErrNoConfig` style.

## Out of Scope (YAGNI)

- `council init -p cheap` to bootstrap a profile from templates. Document copy + edit instead. Add later if friction proves real.
- Profile name in session folder name. Snapshot already records the actual profile.
- Per-profile prompt directories. Profiles override `prompt_file:` paths in their YAML if they want different prompts.
- Listing available profiles (`council profiles ls`). Trivial to add later if needed.
- Validating the picked profile's `name:` field matches the `-p` value. The two are independent — `-p cheap` loading a file with `name: production` is the user's choice, no collision risk.

## Implementation Steps

### Task 1: pkg/config — `LoadByName`, name regex, path mode, scoped error

- [ ] update `pkg/config/loader_test.go`:
  - `TestLoadByName_NameMode_LocalWins` — both `<cwd>/.council/cheap.yaml` and `<home>/.config/council/cheap.yaml` exist; expect cwd version, source path matches.
  - `TestLoadByName_NameMode_HomeFallback` — only home file exists; expect home version, source path matches.
  - `TestLoadByName_NameMode_NoFallbackToEmbeddedForNonDefault` — neither file exists; expect typed `ErrProfileNotFound`, error message names both candidate paths and the requested name.
  - `TestLoadByName_NameMode_DefaultStillEmbeds` — `LoadByName(cwd, "default")` with no on-disk file returns the embedded profile, source `SourceEmbedded`.
  - `TestLoadByName_NameMode_InvalidName` — table of bad names (`..`, `foo/bar`, `.hidden`, `foo bar`, empty string, `-leading-dash`); expect a single `ErrInvalidProfileName` (or descriptive error) and no filesystem access.
  - `TestLoadByName_PathMode_AbsolutePath` — pass an absolute path to a tempfile; loads it.
  - `TestLoadByName_PathMode_RelativePath` — pass `./fixtures/foo.yaml` from a tempdir; loads it.
  - `TestLoadByName_PathMode_BareYAMLSuffix` — pass `cheap.yaml` in cwd; recognised as path (ends in `.yaml`).
  - `TestLoadByName_PathMode_Missing` — pass a path that does not exist; expect a wrapped `os.PathError` (no profile-not-found, no embedded fallback).
  - `TestLoad_StillCallsLoadByNameDefault` — keep one test asserting `Load(cwd)` is `LoadByName(cwd, "default")` to lock the wrapper contract.
- [ ] add `pkg/config/loader.go`:
  - `var profileNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)`
  - `var ErrInvalidProfileName = errors.New("invalid profile name")`
  - `var ErrProfileNotFound = errors.New("profile not found")` (replaces `ErrNoConfig` for the name-mode misses; keep `ErrNoConfig` as an alias for backward source-compat or remove it — see note).
  - `func LoadByName(cwd, name string) (*Profile, string, error)` implementing:
    1. If `name` is path-shaped (`strings.ContainsRune(name, '/') || strings.HasSuffix(name, ".yaml")`) → `LoadFile(name)`; on success return path as source; on `os.IsNotExist` wrap with explanatory message.
    2. Else validate against `profileNameRE`; on miss return `ErrInvalidProfileName`.
    3. Walk precedence: `<cwd>/.council/<name>.yaml` → `<home>/.config/council/<name>.yaml` (preserve existing stat-error handling: surface anything except `ErrNotExist`).
    4. If `name == "default"` and both misses → `loadFromEmbedded()` + `SourceEmbedded`.
    5. Else return `fmt.Errorf("%w %q (checked %s, %s)", ErrProfileNotFound, name, local, global)`.
- [ ] refactor `func Load(cwd string)` to `return LoadByName(cwd, "default")`.
- [ ] note: `ErrNoConfig` — its message mentions the precedence paths verbatim. Leave it in place but mark deprecated in a comment, since the new error carries the same information per-name. No callers outside this package, so removing it would be safe; keep the symbol to avoid a breaking export change. **Decision deferred to implementation — if the `cmd/council` callsite never used the symbol, just delete it.**
- [ ] `go test ./pkg/config/...` must pass before Task 2.

### Task 2: cmd/council — wire `LoadByName`, drop equality gate, update CLI tests

- [ ] update `cmd/council/main_test.go`:
  - Delete `TestRun_NonDefaultProfileRejected` (line 182).
  - Add `TestRun_NamedProfile_LocalHit` — `t.TempDir()`, write `.council/cheap.yaml` (helper analogous to `withCouncilDir`), `-p cheap "q"`; expect exit 0; verbose-mode assertion that the start line says `profile: cheap`.
  - Add `TestRun_NamedProfile_Missing` — `-p cheap "q"` with no file; expect exit 1 and stderr containing `profile not found "cheap"` (or whatever final wording lands).
  - Add `TestRun_PathProfile_Hit` — write a profile to `<tmp>/foo.yaml`; `-p <abs path> "q"`; expect exit 0.
  - Add `TestRun_PathProfile_Missing` — `-p ./does-not-exist.yaml`; expect exit 1 with stderr referencing the path.
  - Add `TestRun_InvalidProfileName` — `-p ../etc/passwd`; expect exit 1 with stderr containing `invalid profile name`.
  - Add `TestRun_DefaultStillEmbeds` — no flag, no on-disk profile (existing helper or just an empty `.council/`); expect exit 0 (embedded fallback works).
- [ ] update `cmd/council/main.go`:
  - Delete the `if profileName != "default"` gate (lines 142–145).
  - Replace the `config.Load(cwd)` call with `config.LoadByName(cwd, profileName)`.
  - Update the error path so a `LoadByName` failure prints a single self-explanatory line; for `ErrProfileNotFound` it should suggest `council init` only when `name == "default"` (because that's the only case `init` can fix).
- [ ] update `cmd/council/main.go` flag help (lines 96–97):
  - `"Profile name (resolves to <name>.yaml under .council/ or ~/.config/council/) or path to a YAML profile."`
- [ ] update `printHelp` text (search `printHelp` to find the multi-line usage block) — short note: `-p default` (default), `-p cheap` to use `~/.config/council/cheap.yaml`, `-p ./foo.yaml` for an explicit path.
- [ ] `go test ./...` must pass before Task 3.

### Task 3: README + CHANGELOG

- [ ] README.md line 131 — replace the "v2 accepts only `-p default`" line with a short subsection (after the existing section on `-p`):

  > **Profiles.** `-p NAME` looks up `<NAME>.yaml` at `<cwd>/.council/` first, then `~/.config/council/`. `-p` also accepts a direct path (anything containing `/` or ending in `.yaml`). To add a profile, copy `~/.config/council/default.yaml` to e.g. `cheap.yaml` and edit the `model:` lines. Names must match `[a-zA-Z0-9][a-zA-Z0-9_-]*`.

- [ ] CHANGELOG.md — under Unreleased: `- council: -p / --profile now accepts arbitrary profile names (resolved to <name>.yaml at the existing precedence locations) and explicit YAML paths (anything containing /  or ending in .yaml). Embedded fallback still applies to -p default only.`
- [ ] no test gate; pure docs.

## Post-Completion

- Manual smoke: write `~/.config/council/cheap.yaml` with the haiku/gpt-mini executor lineup (user does this), run `./council -p cheap "test"` and confirm verbose start line names `cheap` and the run uses the cheap models.
- If we end up wanting `council init -p cheap`, file a follow-up plan; do not bolt it on here.
