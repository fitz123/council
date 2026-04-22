# v2 Debate Engine — Implementation Plan

## Overview

Implement the v2 debate engine on top of the v1 MVP code. v2 adds multi-round anonymized debate (blind R1 + peer-aware R2) followed by distributed voting among N experts. Winner's R2 text is returned verbatim; on 1-1-1 three-way tie, all tied outputs are surfaced. No classifier, no judge, no tournament.

The Round 5 simplified design removes single-model bias by eliminating the judge role — voting distributes the final decision across all experts (D15). A 16-hex per-session nonce stops forged-fence injection across the LLM-output boundary (D11, ADR-0008). An explicit `council resume` subcommand (D14) lets operators continue interrupted sessions without full reruns.

**Net v2 delta vs v1:** debate rounds + anonymization + nonce + vote stage + resume. Single profile, single flow.

## Context (from discovery)

- **Source of truth:** `docs/design/v2.md` (§3 decisions D1–D3, D5–D8, D10–D12, D14–D16; §5 research map; §6 success criteria and exit codes; §7 fitness functions).
- **Technical spec derivative:** `docs/plans/v2-debate-engine.md` — has the detailed schema, folder layout, orchestration algorithm, verdict shape.
- **ADRs:** ADR-0008 (accepted, debate rounds + anonymization + nonce). ADR-0007 and ADR-0009 are superseded — read them only for history.
- **v1 code base** (already on main via PR #3, `feat/v1-mvp-implementation`):
  - `pkg/config/` — loader, embedded FS, snapshot
  - `pkg/session/` — session ID, folder ops, verdict.WriteAtomic
  - `pkg/runner/` — subprocess runner (process-group kill, retries, 429 handling)
  - `pkg/executor/` — Executor interface, claude-code impl, registry
  - `pkg/prompt/` — expert + judge prompt builders (accepts optional `prior_rounds` — forward-compat hook for v2)
  - `pkg/orchestrator/` — single-pass fan-out, quorum, judge, verdict assembly
  - `cmd/council/` — CLI entry
  - `defaults/` — embedded v1 `default.yaml` + prompts
  - `test/smoke/` — F1–F7 smoke suite
- **New package to create:** `pkg/debate/` — rounds, anonymization, voting. Internal file split: `anonymize.go`, `rounds.go`, `vote.go`.
- **Key contracts to preserve:**
  - v1 CLI (council "q", -p, --profile, -, -v) still works
  - Executor interface unchanged
  - pkg/runner unchanged (process-group kill, 429 retry)
  - Atomic verdict.json write unchanged
  - Folder-as-database (ADR-0003)

## Development Approach

- **Testing approach: TDD** — tests first, see them fail, implement, see them pass.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- Run tests after each change.
- Maintain backward compatibility with v1 CLI and exit codes.

## Testing Strategy

- **Unit tests:** required for every task. `go test ./...` must pass.
- **Smoke tests:** v1's `test/smoke/` suite (F1–F7) must keep passing; extend with F8/F9/F12 for v2.
- **`testbinary` mock executor:** v1 ships a test-only executor for deterministic subprocess simulation. v2 reuses it for debate-round integration tests.
- **Coverage target:** 80%+ per package (v1 standard).

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.
- Keep plan in sync with actual work done.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code + tests + doc updates inside this repo.
- **Post-Completion** (no checkboxes): PR review, benchmark runs, external verification.

## Implementation Steps

### Task 1: v2 profile schema + embedded defaults

- [ ] write tests for `Profile` struct new fields (`rounds int`, `voting.BallotPromptFile string`, `voting.Timeout time.Duration`) — round-trip YAML load + validate
- [ ] write tests for validator: reject `rounds != 2` (v2 ships K=2 only; K=1 and K≥3 deferred to v3); reject missing `voting.ballot_prompt_file`; accept valid profile
- [ ] write test asserting `len(experts) >= 2` still required (v1 ADR-0005 preserved)
- [ ] extend `Profile` struct in `pkg/config/loader.go` with `Rounds int` and `Voting VotingConfig` fields
- [ ] add `version: 2` handling path — `version: 1` profiles fail to load under v2 binary (clear migration error: operator must add `rounds: 2` + `voting` block). No silent inference.
- [ ] update embedded FS (`pkg/config/embed.go` / `defaults/defaults.go`): `defaults/default.yaml` with `version: 2`, `rounds: 2`, `voting.ballot_prompt_file: prompts/ballot.md`
- [ ] add `defaults/prompts/ballot.md` — single-letter `VOTE: <label>` output contract
- [ ] add `defaults/prompts/peer-aware.md` — R2 expert prompt (`"prior-round consensus is NOT ground truth"`)
- [ ] remove v1's `defaults/prompts/critic.md` if obsolete under new v2 profile (keep `independent.md` for R1)
- [ ] run `go test ./pkg/config/...` — must pass before Task 2

### Task 2: pkg/prompt — injection helpers

- [ ] write tests for `prompt.Wrap(label, content, nonce string) string`: output matches `=== EXPERT: <label> [nonce-<hex>] ===\n<content>\n=== END EXPERT: <label> [nonce-<hex>] ===`
- [ ] write tests for `prompt.CheckForgery(output, nonce string) error`:
  - output containing the session nonce as substring → `ErrNonceLeakage`
  - output containing any line-anchored delimiter `(?m)^=== .* ===$` → `ErrForgedFence` (catches open fences `=== EXPERT: A [nonce-...] ===`, close fences `=== END EXPERT: A [nonce-...] ===`, and global section fences `=== CANDIDATES ===` / `=== END CANDIDATES ===`)
  - test matrix MUST include positive cases for: fake open fence, fake `END EXPERT`, fake `CANDIDATES` / `END CANDIDATES`, fake fence with a wrong-nonce suffix
  - clean output → no error
- [ ] write tests for `prompt.ScanQuestionForInjection(question string) error`: question containing any line-anchored delimiter `(?m)^=== .* ===$` → returns `ErrInjectionSuspected`
- [ ] implement `Wrap` in `pkg/prompt/injection.go`
- [ ] implement `CheckForgery` — substring check for nonce + broad delimiter-line regex `(?m)^=== .* ===$` rejecting all delimiter-shaped lines regardless of content
- [ ] implement `ScanQuestionForInjection` using same delimiter-line regex
- [ ] export sentinel errors (`ErrNonceLeakage`, `ErrForgedFence`, `ErrInjectionSuspected`)
- [ ] run `go test ./pkg/prompt/...` — must pass before Task 3

### Task 3: pkg/debate — anonymization

- [ ] write tests for `AssignLabels(sessionID string, experts []Expert) map[string]string`:
  - deterministic: same sessionID + same experts → same mapping (run twice, assert equal)
  - shuffle happens: for different sessionIDs, mapping varies
  - labels are `A, B, C, …` in the shuffled order
  - N=3 case: returns exactly 3 entries mapping single-letter labels to real names
- [ ] create `pkg/debate/anonymize.go`
- [ ] implement `AssignLabels`: compute `digest := sha256.Sum256([]byte(sessionID))`; derive `seedHi := binary.BigEndian.Uint64(digest[0:8])`, `seedLo := binary.BigEndian.Uint64(digest[8:16])`; `rng := rand.New(rand.NewPCG(seedHi, seedLo))`; shuffle experts via `rng.Shuffle`; assign labels A, B, C in shuffled order
- [ ] constrain N ≤ 26 (return error if exceeded — v3 extension via `A1, A2, …` deferred)
- [ ] export reverse-lookup helper `LabelOf(real string, mapping map[string]string) (label string, ok bool)` for orchestrator convenience
- [ ] run `go test ./pkg/debate/...` — must pass before Task 4

### Task 4: pkg/debate — session nonce generation + snapshot persistence

- [ ] write test: `GenerateNonce() string` returns 16 lowercase hex chars `[0-9a-f]{16}`; different calls return different nonces (high-probability assertion, 8-byte entropy)
- [ ] write test: nonce is persisted in `profile.snapshot.yaml` via `pkg/session` (extend session snapshot loader to include `session_nonce`)
- [ ] implement `pkg/debate.GenerateNonce()` using `crypto/rand.Read(8 bytes)` + hex encode
- [ ] extend `pkg/session/session.go` snapshot writer to include `session_nonce` field in `profile.snapshot.yaml`
- [ ] extend `pkg/session/session.go` snapshot loader (`LoadSnapshot`) to re-read `session_nonce` (for resume in Task 11)
- [ ] run `go test ./pkg/debate/... ./pkg/session/...` — must pass before Task 5

### Task 5: pkg/debate — R1 blind fan-out

- [ ] write test for `RunRound1(ctx, cfg RoundConfig, question string) ([]RoundOutput, error)` using `executor/mock`:
  - N=3 experts, all succeed → returns 3 RoundOutputs with `Participation == "ok"`
  - one expert times out, retries exhausted → dropped, `Participation == "failed"`
  - one expert produces output containing the session nonce → rejected, marked failed
  - output written to `rounds/1/experts/<label>/output.md` per surviving expert
  - `.done` marker written only for successful experts
- [ ] write test: `surviving_count < quorum` → `RunRound1` returns a specific error (`ErrQuorumFailedR1`) so orchestrator can exit 2
- [ ] create `pkg/debate/rounds.go` — `RunRound1` function signature + implementation
- [ ] use existing `pkg/runner` primitive for subprocess spawn (DO NOT duplicate)
- [ ] wire forgery check via `prompt.CheckForgery` — on match, treat expert as failed for this round
- [ ] write session-folder artifacts via `pkg/session` helpers (DO NOT duplicate folder creation)
- [ ] run `go test ./pkg/debate/...` — must pass before Task 6

### Task 6: pkg/debate — R2 peer-aware fan-out + carry-forward

- [ ] write test for `RunRound2(ctx, cfg RoundConfig, question string, r1 []RoundOutput) ([]RoundOutput, error)` using `executor/mock`:
  - peer aggregate for each expert excludes self; orders alphabetically by label
  - each peer output is wrapped via `prompt.Wrap` with session nonce
  - expert fails in R2 → R1 output copied to `rounds/2/experts/<label>/output.md`, `Participation == "carried"`
  - expert succeeds → R2 output written, `Participation == "ok"`
  - surviving_count < quorum after R2 → `ErrQuorumFailedR2`
- [ ] write test: R2 aggregate file `rounds/2/aggregate.md` is written ONCE after final R2 completes, contains all surviving experts' R2 outputs wrapped with nonce, ordered alphabetically by label
- [ ] implement `RunRound2` in `pkg/debate/rounds.go`
- [ ] implement `buildPeerAggregate(forLabel string, outputs []RoundOutput, nonce string) string` helper — excludes `forLabel`, orders alphabetically, wraps each with `prompt.Wrap`
- [ ] implement `writeGlobalAggregate(session *Session, round int, outputs []RoundOutput, nonce string) error` — writes `rounds/N/aggregate.md`
- [ ] run `go test ./pkg/debate/...` — must pass before Task 7

### Task 7: pkg/debate — voting stage

- [ ] write tests for `RunBallot(ctx, cfg BallotConfig, question, aggregateMD string) ([]Ballot, error)`:
  - N=3 experts all produce `VOTE: A` / `VOTE: B` / `VOTE: C` → 3 Ballots
  - malformed ballot (no VOTE line) → voter's ballot discarded, NOT a run failure
  - ballot votes for non-existent label (e.g., votes `D` when only A/B/C survived) → discarded
  - ballot is a fresh subprocess (no prior-round context) — verified by checking prompt passed to mock executor
- [ ] write tests for `Tally(ballots []Ballot, activeLabels []string) TallyResult` — general form (see design D8):
  - **Full cohort:** 3-0-0 → `Winner == "A"`, `TiedCandidates == nil`; 2-1-0 → unique winner; 1-1-1 → `Winner == ""`, `TiedCandidates == ["A","B","C"]` (sorted)
  - **Reduced cohort (N=2 post-R1-drop):** 2-0 → unique winner; 1-1 → `TiedCandidates == ["A","B"]`
  - **Malformed ballots discarded:** 3 votes with 1 malformed → tally from 2 valid ballots; if they split 1-1 → tie; if they agree 2-0 → winner
  - **All malformed (zero valid ballots):** every active label is "tied at max zero" → `TiedCandidates` lists every active label; `Winner == ""`
  - invariant: if `TiedCandidates != nil`, every listed label is in `activeLabels` and has the max vote count (possibly zero)
- [ ] write tests for `SelectOutput(session *Session, result TallyResult, r2 []RoundOutput) error`:
  - unique winner → copies winner's `rounds/2/experts/<winner>/output.md` to session root `output.md`
  - N-way tie (any N ≥ 2 tied candidates) → copies each tied label's R2 output to `output-<label>.md` (e.g., `output-A.md`, `output-B.md`, ...)
- [ ] create `pkg/debate/vote.go` with `RunBallot`, `Tally`, `SelectOutput`
- [ ] ballot parser regex: `(?m)^VOTE: ([A-Z])$` with active-label validation
- [ ] Tally iteration order must be stable: sort active labels alphabetically before computing max (determinism)
- [ ] write `voting/tally.json` and per-voter `voting/votes/<voter-label>.txt` via `pkg/session`
- [ ] run `go test ./pkg/debate/...` — must pass before Task 8

### Task 8: Orchestrator integration

- [ ] write integration test in `pkg/orchestrator/orchestrator_test.go` using `executor/mock`:
  - happy path: N=3, K=2, all succeed → session folder has rounds/1, rounds/2, voting/, output.md, verdict.json with `status: ok`
  - one expert fails R1 → quorum=1 still satisfied, run continues with N=2 through R2 + vote
  - 1-1-1 tie → output-A.md + output-B.md + output-C.md present, exit 2 `no_consensus`
  - injection in question → exit 1 `injection_suspected_in_question`
- [ ] extend `pkg/orchestrator/orchestrator.go` `Run()`:
  - session setup (session_id, nonce, anonymization, snapshot write, question sanity scan)
  - R1 via `debate.RunRound1`
  - quorum check after R1
  - R2 via `debate.RunRound2` (if K >= 2)
  - quorum check after R2
  - write global aggregate
  - vote via `debate.RunBallot`
  - tally + output selection
  - verdict write (atomic)
  - **root-level `.done` marker written AFTER verdict.json lands** — this marker is the finality signal consumed by `council resume` (D14). SIGINT handler must NOT write root `.done`; it writes `verdict.json` with `status: "interrupted"` only.
- [ ] preserve v1 exit codes; add exit 2 `no_consensus` for tie; no exit 3 (no judge); no exit 4 (no classifier)
- [ ] run `go test ./pkg/orchestrator/...` — must pass before Task 9

### Task 9: verdict.json v2 schema

- [ ] write test for verdict v2 shape (matches `docs/plans/v2-debate-engine.md` §12):
  - `version == 2`
  - `rounds` array with per-round expert entries (label, real_name, participation, duration)
  - `experts[].participation_by_round` equals `(.rounds | length)` array of `ok`/`carried`/`failed`
  - `voting.votes` object, `voting.winner` OR `voting.tied_candidates` (exactly one)
  - `voting.ballots` array of {voter_label, voted_for}
  - `anonymization` map `{label: real_name}`
  - `status == "ok"` or `"no_consensus"`
- [ ] write fitness F8 assertion test: anonymization consistency — every (label, real_name) in rounds[] matches anonymization map
- [ ] write fitness F12 test: exactly one of `voting.winner` / `voting.tied_candidates` is populated
- [ ] write fitness F7 test: `participation_by_round | length == (.rounds | length)` (K-agnostic)
- [ ] extend `pkg/session/verdict.go` with v2 shape (add `VerdictV2` struct or extend existing with `version: 2`)
- [ ] atomic write via `pkg/session/verdict.WriteAtomic` preserved
- [ ] run `go test ./pkg/session/... ./pkg/orchestrator/...` — must pass before Task 10

### Task 10: `council resume` subcommand (D14)

- [ ] write tests for `pkg/session.FindIncomplete(root string) (sessionPath string, err error)` — finality-based predicate per design D14:
  - no folders → `ErrNoResumableSession`
  - folder with root-level `.done` marker → skipped (final)
  - folder with `verdict.json.status ∈ {ok, no_consensus, quorum_failed_round_1, quorum_failed_round_2, injection_suspected_in_question, config_error}` → skipped (final)
  - folder with no .done markers anywhere → skipped (nothing progressed)
  - folder with `rounds/1/experts/A/.done` but no verdict.json → **returned** (resumable)
  - folder with partial `verdict.json.status == "interrupted"` + stage `.done` markers → **returned** (resumable; this is the SIGINT-mid-run case that F6 + ADR-0008 F5 expect to resume)
  - multiple resumable → newest wins
- [ ] write tests for `pkg/session.LoadExisting(path string) (*Session, error)`: re-derives anonymization from session_id, re-reads session_nonce from snapshot, restores profile + question
- [ ] write integration test for resume:
  - full run crashes after R2 (simulate via mock executor returning "success" for R1/R2 experts but kill before vote) — resume should run only the vote stage + finalize
  - crash mid-R2 (one expert done, others not) — resume re-spawns missing experts in R2
  - **SIGINT mid-R2 with partial verdict.json** — send SIGINT while R2 experts are running; v1 SIGINT handler writes `verdict.json` with `status: "interrupted"` + anonymization map; resume must pick this session up despite verdict.json existing (verifies finality-not-existence predicate)
- [ ] implement `FindIncomplete` in `pkg/session/resume.go`
- [ ] implement `LoadExisting` in `pkg/session/resume.go`
- [ ] add `resume` subcommand to `cmd/council/main.go` with `--session <id>` flag
- [ ] wire resume path: determine first incomplete stage, call appropriate `debate.*` function with previously-loaded state, continue to finalize
- [ ] exit 1 `no_resumable_session` if nothing to resume
- [ ] run `go test ./pkg/session/... ./cmd/council/...` — must pass before Task 11

### Task 11: Smoke test suite + documentation updates

- [ ] extend `test/smoke/smoke_test.go` with F8 (anonymization consistency), F9 (forgery detection via unit test), F12 (vote outcome) assertions
- [ ] update `test/smoke/run.sh` or equivalent to exercise v2 flows (happy path + tie path + resume path)
- [ ] verify smoke suite passes: `go test -tags=smoke ./test/smoke/...`
- [ ] update `README.md`: v2 is a debate engine; `council "q"` runs R1+R2+vote; `council resume` exists; bump version bullet (v2 bumps `verdict.json.version` to 2)
- [ ] update `README.md` CLI examples — remove references to v1 single-pass phrasing where it misleads
- [ ] add a "what's new in v2" paragraph pointing at `docs/design/v2.md`
- [ ] run full suite: `go test ./... && go test -tags=smoke ./test/smoke/...`

### Task 12: Verify acceptance criteria

- [ ] verify all D1–D3, D5–D8, D10–D12, D14–D16 decisions from `docs/design/v2.md` §3 are reflected in code (map each decision → relevant test)
- [ ] verify fitness functions F1–F8, F9, F12 from `docs/plans/v2-debate-engine.md` §15 all have tests
- [ ] verify exit codes match `docs/plans/v2-debate-engine.md` §13 (0 / 1 / 2 / 130; no 3 or 4)
- [ ] run full test suite one more time
- [ ] run linter (`golangci-lint run` or `go vet ./...`) — all issues resolved
- [ ] verify test coverage ≥ 80% in new pkg/debate and modified pkg/orchestrator, pkg/session, pkg/prompt, pkg/config packages (use `go test -cover`)
- [ ] scan for TODOs / FIXMEs introduced by implementation — resolve or file follow-ups

## Technical Details

### Session folder layout (v2 extension of v1)

```
.council/sessions/<petname-id>/
├── question.md
├── profile.snapshot.yaml      # includes session_nonce field
├── rounds/
│   ├── 1/experts/<A|B|C>/{prompt.md, output.md, stderr.log, .done}
│   └── 2/
│       ├── experts/<A|B|C>/{prompt.md, output.md, stderr.log, .done}
│       └── aggregate.md        # global aggregate of all R2 outputs (ballot input)
├── voting/
│   ├── votes/<A|B|C>.txt       # "VOTE: B"
│   └── tally.json
├── output.md                   # winner case
│  OR
├── output-A.md                 # 1-1-1 tie case
├── output-B.md
├── output-C.md
├── verdict.json                # v2 schema
└── .done
```

### Nonce scope

All LLM-sourced text wrapped with `[nonce-<hex>]` fences before being fed into downstream prompts:
- R1 outputs → R2 peer aggregates
- R2 outputs → ballot input (`rounds/2/aggregate.md`)

Operator question is NOT fenced but IS sanity-scanned at load time for `^=== ` patterns.

### Ballot contract

```
=== USER QUESTION ===
<question>
=== END USER QUESTION ===

=== CANDIDATES ===
=== EXPERT: A [nonce-...] ===
<A's R2 output>
=== END EXPERT: A [nonce-...] ===

=== EXPERT: B [nonce-...] ===
...
=== END EXPERT: B [nonce-...] ===

...

=== END CANDIDATES ===

Pick the label whose answer is best. Output ONLY the line: VOTE: <label>
```

Expert parses its own answer back: regex `(?m)^VOTE: ([A-Z])$`. Validates against active label set.

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only.*

**Manual verification:**
- Run `council "what is 2+2?"` from a plain shell (not inside an active `claude` session) and verify a real answer is returned via the debate+vote flow.
- Run `council "Raft or Paxos?"` on a question likely to split the council; observe whether a tie arises (should be rare).
- Verify `council resume` after killing a real run mid-R2 via SIGINT — expect the session to complete without data loss.
- Inspect `verdict.json` structure and ensure downstream tools (if any) handle the v2 shape.

**External system updates:**
- After PR merge, update README install snippet if any Go version bumped.
- Verify GitHub Actions / CI picks up v2 smoke tests.

**Non-goals flagged for v3** (reference design §5):
- Multi-vendor executors (Codex, Gemini)
- Classifier / structured-output schemas
- Judge polish step (if operational data shows raw text too rough)
- Tiebreak mechanism for 1-1-1 (if operational data shows ties common)
- Ranking / rating / numerical-estimation modes
- Per-section SHA-256 injection boundaries
