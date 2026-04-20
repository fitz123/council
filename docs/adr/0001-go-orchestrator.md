# ADR-0001: Go binary orchestrator, no LLM in the decision loop

**Status:** Proposed.

## Context

Council needs an orchestrator that fans out questions to expert CLIs in parallel, waits for results with timeouts, handles retries, synthesizes via a judge, and writes artifacts. The orchestrator itself should be deterministic, cheap to run, and easy to debug.

Alternatives considered:

- **Shell script** — struggles with parallelism, timeouts, and structured state. Not viable at the complexity level council needs.
- **LLM meta-agent orchestrator** (an agent controlling other agents) — non-deterministic, consumes tokens for pure control flow, and prior art (ralphex) documents LLM orchestrators repeatedly failing to reliably launch downstream agents per written procedure.
- **Python** — simpler to write, worse single-binary distribution story (packaging, interpreter version pinning).
- **Go** — excellent parallelism (goroutines), single static binary, strong stdlib (`os/exec`, `context`, `os/signal`), zero runtime dependency.

## Decision

Council orchestrator is written in Go 1.23+ and distributed as a single binary via `go install`. LLMs are invoked only inside role-specific subprocesses (experts, judge), never in the orchestration path.

## Consequences

- **(+)** Deterministic: identical input → identical action sequence.
- **(+)** Single-binary distribution, no runtime dependency.
- **(+)** Pattern alignment with ralphex (reference implementation for multi-agent orchestration).
- **(−)** No "intelligent" orchestration (adaptive timeouts, re-routing on upstream failure). Deferred to v2 if needed.
- **(−)** Go-specific idioms may be less approachable than Python for contributors unfamiliar with the ecosystem.
