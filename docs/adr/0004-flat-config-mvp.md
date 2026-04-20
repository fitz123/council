# ADR-0004: Flat single-file config for MVP (defer directory split)

**Status:** Proposed.

## Context

Initial design considered separate directories for profiles, experts, and judges — one file per concept, reusable across profiles.

For MVP (one profile, two expert personas, one judge — see ADR-0005), this adds ceremony without benefit:

- 5+ files for a trivially small configuration.
- Three places to look when debugging a single run's config.
- Overhead for a tool whose config is read once per invocation, not edited frequently.

Alternatives:

- **Multi-directory layout (profiles/, experts/, judges/)** — clean abstraction, justified when profiles reuse expert definitions across ≥3 profiles.
- **Flat single YAML with inline prompt bodies** — everything in one file. Breaks markdown tooling for long prompts.
- **Flat single YAML referencing prompt files by path** — one config file, prompts stay as standalone `.md` files.

## Decision

MVP uses one `defaults/default.yaml` with `experts:` and `judge:` sections, each referencing `prompt_file` paths (resolved relative to the config file). Prompts are `defaults/prompts/*.md`.

When the system gains ≥2 profiles or ≥3 experts worth sharing across profiles, the schema is extended to also accept:

```yaml
extends: shared/experts.yaml
```

and a `profiles/` / `experts/` / `judges/` directory layout, with the flat form as a fallback. No schema-break — extension only.

## Consequences

- **(+)** Four files cover the MVP config (one YAML + three prompts: independent, critic, judge).
- **(+)** Debugging the exact config of a run is trivial — `cat profile.snapshot.yaml`.
- **(−)** When the catalog of expert personas grows, the flat file bloats. The signal to migrate to directories is explicit (≥3 experts or ≥2 profiles).
- **(−)** If a user manually splits into directories in v1, the loader won't see their files. Documented in README.
