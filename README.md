# council

Multi-expert CLI committee. Fan out one question to N expert CLI-instances, a judge synthesizes one final answer. Every run is archived on disk as file artifacts for audit.

**Status:** pre-MVP — design docs under review. No working code yet.

## Why

Getting a single opinion from a single LLM is noisy. Running the same question through multiple expert personas and synthesizing the results is the ralphex pattern applied to general-purpose questions, not just code reviews.

Inspired by:
- [umputun/ralphex](https://github.com/umputun/ralphex) — deterministic Go orchestrator, multi-agent review pipeline.
- [DenisSergeevitch/repo-task-proof-loop](https://github.com/DenisSergeevitch/repo-task-proof-loop) — durable task-folder pattern, evidence-as-artifacts.
- Multi-LLM council pattern — fan-out to N experts + judge synthesizes final answer.

## Design

- `docs/spec/` — the MVP v1 spec.
- `docs/adr/` — architectural decision records.
- `docs/architect-review.md` — systems-architect methodology review of the spec.

## License

MIT. See [LICENSE](LICENSE).
