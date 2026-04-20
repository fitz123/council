# council v1 — Architectural Review

**Scope:** Additive architectural critique of the v1 MVP spec (see `docs/spec/v1.md`). Not a rewrite — captures the systems-analysis reasoning behind the six ADRs in `docs/adr/`.

**Method:** Problem-space → solution-design → fitness functions → risk register.

---

## Part A — Problem Space

### Stakeholders

| Stakeholder | Influence | Interest | Strategy | Concerns |
|---|---|---|---|---|
| **Owner / operator** (single-user, local machine) | High | High | Manage closely | Works on current OS, fits existing workflow, no hidden API costs, trivial to restart, debuggable via file artifacts |
| **Upstream CLI vendor** (`claude -p` for MVP; later `codex`, `gemini-cli`) | High | Low | Keep satisfied | Flag surface, rate limits, subscription usage — hard external constraint |
| **Future OSS users** | Low | Mid | Inform | README quality, install UX, config discoverability |
| **Adjacent tooling** (ralphex, proof-loop) | Low | Low | Monitor | Pattern alignment, potential cross-pollination |

The owner/operator is the only stakeholder with veto. The OSS audience is a *future* concern — it must not dictate MVP shape (classic stakeholder-matrix trap).

### Quality attributes

**Explicit** (from spec "success criteria"):
- **Performance** — simple question < 60s end-to-end.
- **Testability** — smoke tests pass, distinct exit codes, non-colliding sessions.
- **Simplicity** — one binary, one Go build, minimal deps.

**Implicit** (company-lifecycle = personal-tool / early-prototype phase):
- **Modifiability** — the profile schema *will* change; debate rounds and non-Claude CLIs will land later. Architecture must tolerate churn.
- **Maintainability** — solo operator; an artifact on disk beats "reproduce from logs" six months later.
- **Observability** — covered by the artifact-as-log pattern (implicit fitness function).
- **Deployability** — `go install ...@latest`; no service, no daemon, no plist.

Deliberately **not** priorities at MVP: scalability, availability, security (single-user, local). Stating this explicitly prevents scope creep — MVP is not a multi-tenant service.

### External constraints

- **Upstream subscription model** — per-request calls share a single quota. Parallel expert fan-out hits the same account → implicit throttling ceiling. Quorum/retry must distinguish 429 from timeout.
- **POSIX target** — process-group kill on SIGTERM behaves; file-locking semantics are standard; the `.done` marker pattern is safe.
- **Nested-CLI hazard** — a council binary that invokes `claude -p` cannot itself be spawned from inside an active `claude` session without recursion risk. This is a deployment constraint, not a code constraint — documented in the spec.
- **No API fallback** — council cannot reach around a broken CLI to a raw SDK. If the CLI contract breaks, council breaks.

### Requirements gaps flagged by the review

- **Rate-limit behavior was unspecified.** A 429 from the upstream CLI is not a timeout — it's throttling. Retry-once with no backoff wastes the second slot. → Classify 429 as a distinct failure kind; document as known v1 limitation.
- **No SLO for session-folder disk growth.** After many runs, `.council/sessions/` accumulates. Retention/rotation is absent — address in README, add `council gc` in v2.
- **No concurrent-invocation contract.** Petname-suffixed session IDs prevent folder collisions; nothing else is shared. Document explicitly.

---

## Part B — Solution Design

### Bounded contexts (solution-space)

1. **CLI surface** — arg parsing, exit codes, stdout/stderr formatting.
2. **Config loader** — resolve profile path, load YAML, validate schema, snapshot.
3. **Session writer** — folder creation, ID generation, verdict writer.
4. **Subprocess runner** (shared primitive) — spawn, stdin-pipe, timeout, process-group kill, retry, `.done` marker, stderr capture-on-failure.
5. **Expert role** — thin wrapper: prompt assembly + subprocess runner.
6. **Judge role** — thin wrapper: prompt assembly (with expert outputs inlined behind injection boundaries) + subprocess runner.
7. **Orchestrator** — fan-out goroutines, quorum gate, sequence `experts → judge`.

Boundaries are clean under one condition: **expert and judge must share the subprocess-runner primitive**, not duplicate it. This is a hard contract, not a suggestion.

**Entity-service risk:** none. Contexts are process/role-oriented, not entity-oriented. `pkg/session` manages the session artifact (verdict + folder ops), not a "Session entity".

**Technical-steps-as-services anti-pattern check:** `pkg/expert` and `pkg/judge` look suspicious — both are "run a subprocess with a prompt". Mitigated by treating them as thin role wrappers over the shared primitive.

### Architectural style

Council is a **Pipeline** (fan-out variant): `question → [expert₁ ‖ expert₂ ‖ … ‖ expertₙ] → judge → verdict`.

- *Pipeline anti-pattern* "state between filters" — clean. Each expert is stateless; the judge sees immutable outputs.
- *Pipeline anti-pattern* "filter with multiple responsibilities" — risk for the runner if it accrues config loading, notifications, or exit-code mapping. Keep it narrow.

A secondary style hint: **Microkernel**. Experts and judges are plugins (prompt + executor type); the orchestrator is the kernel. For MVP the pipeline framing dominates; the microkernel framing becomes real in v2 when `executor: codex | gemini-cli` lands. Naming the executor boundary now keeps that transition cheap.

### Coupling analysis

| Pair | Coupling types | Assessment |
|---|---|---|
| Orchestrator ↔ Config | Static + Semantic (schema shape) | Unavoidable. Profile snapshot (`profile.snapshot.yaml`) breaks temporal coupling — once snapped, config edits cannot affect a running session. **Good.** |
| Orchestrator ↔ Expert/Judge role | Dynamic + Semantic (verdict schema) | Normal. |
| Expert role ↔ Judge role | Implementation (shared subprocess primitive) | Desired — deduplicate aggressively. |
| Roles ↔ Upstream CLI | Semantic (flag names, stdout format), Deployment (must be on PATH) | **Hardest external coupling.** Wrap in a single `Executor` interface so swapping CLIs is a file change. |
| Session folder ↔ `verdict.json` consumers | Static + Semantic | `verdict.json` is the sole machine-readable contract; freeze the schema early; version it (`"version": 1`). |

No overcoupling. The dominant risk is semantic coupling to the upstream CLI's flag surface — explicit "verify flags" step is part of the MVP task list.

### Communication

Subprocess IPC: stdin (prompt) + stdout (answer) + stderr (diagnostic) + exit code. Synchronous, one-shot. Async is rejected — it contradicts the quorum gate (judge needs all outputs).

### Data model

The session folder **is** the data model:

- **Append-only per session** — no mutation after `.done`.
- **Self-describing** — `verdict.json` is the index; files are the evidence.
- **Single owner per session** — one council process; concurrent sessions are isolated by petname suffix.

`verdict.json` is written last via `tmp + fsync + rename` so a reader sees either nothing or the final file — never a partial.

---

## Part C — ADRs

Six ADRs capture the key decisions (see `docs/adr/`):

- **0001** — Go orchestrator; no LLM in the decision loop.
- **0002** — Subprocess IPC via stdin/stdout/exit codes (not argv — `ARG_MAX` limit).
- **0003** — File-based session artifacts (folder-as-database) with atomic `verdict.json`.
- **0004** — Flat single-file config for MVP; defer the `profiles/experts/judges/` split.
- **0005** — Single model/CLI at MVP (Claude Code); default profile ships N ≥ 2 expert personas.
- **0006** — Judge synthesizes only; no debate rounds at MVP.

Each ADR records alternatives considered and the consequence trade-offs.

---

## Part D — Fitness Functions

Automatable checks for CI or a smoke suite.

1. **Round-trip under 60s (Performance).** `council "what is 2+2?"` exits 0 in under 60s.
2. **`verdict.json` schema validity (Testability).** `jq -e '.version, .session_id, .answer, .rounds[0].experts, .rounds[0].judge'` passes after every test run. Aligned with spec `§16` F2.
3. **Session-folder isolation (Parallel safety).** Three concurrent `council` invocations from one cwd produce three distinct session folders with non-colliding IDs.
4. **Retry increments counter (Reliability contract).** With a stub executor that fails once then succeeds, `verdict.json.rounds[0].experts[*].retries` reflects the retry.
5. **SIGINT produces partial verdict + exit 130 (Robustness).** Sending SIGINT mid-run kills the subprocess tree (no orphan `claude -p`) and writes `verdict.json` with `status: "interrupted"`.
6. **Prompt larger than argv limit (Scale smoke).** A 200 KB question must pass (validates stdin-not-argv).
7. **Config validation rejects unknown fields (Simplicity).** Unknown top-level or per-expert YAML keys produce exit 1 with a readable error. Guards against silent drift.

---

## Part E — Risk Register

Priority: **P1** = blocker for shipping · **P2** = resolve before implementation starts · **P3** = before v2.

| P | Risk / gap | Where it shows up | Mitigation |
|---|---|---|---|
| P1 | Rate-limit (429) not distinguished from timeout | Parallel experts on one subscription | Parse stderr for "rate_limit"/"429"; classify as distinct failure kind; document as v1 limitation |
| P1 | Semantic coupling to upstream CLI flags not verified against a live CLI release | First implementation pass | Run a small experimental script against the current upstream CLI before writing the executor; record verified flag set in the spec |
| P1 | Nested-CLI hazard if council is launched from inside an active `claude` session | Deployment environments that wrap council in LLM sessions | Document deployment constraint: council is launched from a plain shell or cron, not from inside an active `claude` session |
| P2 | Atomic write of `verdict.json` unspecified | Crash during write → readers see partial JSON | `tmp + fsync + rename`; specified in ADR-0003 |
| P2 | Process-group kill semantics on timeout | `SIGKILL` on parent does not reach the upstream CLI child | Spawn with `Setpgid=true`; kill via negative PID |
| P2 | `verdict.json` without `version` field | v2 schema changes break consumers silently | `"version": 1` at root; specified in ADR-0003 |
| P2 | Subprocess primitive duplicated between expert and judge | Drift between two copies | One shared `pkg/runner`; expert/judge are thin role wrappers |
| P3 | Session-folder disk growth without rotation | Long-term use | README "maintenance" note; `council gc --older-than 30d` in v2 |
| P3 | Default expert prompt too terse for complex questions | UX tuning | Observable; not architectural |

---

## Part F — Verdict

The specification is architecturally sound. The pipeline style is correctly chosen, bounded contexts are clean (no Entity Services, no technical-steps-as-services), and the coupling profile is acceptable — the hardest dependency (upstream CLI flag surface) is acknowledged and routed into a must-verify step. The folder-as-database data model is simple, durable, and debuggable — the right choice for a single-user prototype-stage tool.

The three remaining architectural tasks before implementation starts:

1. **Document the shared subprocess-runner primitive** so expert and judge reuse it (P2).
2. **Fix atomic-write semantics for `verdict.json` and process-group kill on SIGINT** (P2).
3. **Verify upstream CLI flags against the current release** and fold the result back into the spec (P1).

After these, the remaining work is tactical — how to implement — not strategic.
