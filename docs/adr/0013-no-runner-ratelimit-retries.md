
# ADR-0013: No Runner-Side Rate-Limit Retries

**Status:** Proposed
**Date:** 2026-04-24
**Amends:** v1.md §10 (retry-ownership split)
**Depends on:** ADR-0012

## Context

v1/v2 `pkg/runner.Run()` retries on 429 up to `RateLimitMaxRetries`
times + classifies stderr into `ErrRateLimit`. Single-CLI council
made this correct — a rate-limited call blocked the whole session;
silent retry let runs succeed.

v3 (ADR-0012) runs each expert on a different vendor. Independent
rate-limit buckets per vendor mean one-vendor rate-limit rarely
correlates with the others being limited. Orchestrator-level quorum
is the right abstraction to absorb single-vendor outage.

Runner-side retry now actively harms:

1. Wastes wall-clock on retries against the one failing vendor
   instead of returning a verdict from survivors.
2. Doubles up inside the CLI — both codex and gemini already retry
   429 internally (codex `RetryOn::retry_429`; gemini
   `retryWithBackoff` in `retry.ts`). Stacking another layer just
   repeats the same failing subprocess.
3. Hides partial-failure diagnostics. Rate-limit is an operationally
   interesting signal (credit out, quota hit); silent retry swallows
   it. Ralphex surfaces typed errors + per-CLI help (see
   `umputun/ralphex` `pkg/processor/runner.go:1191-1207`).

## Decision

Remove the rate-limit retry loop from `pkg/runner`. Remove
classification too — executors own detection via
`runner.DetectLimit(stderrPath, patterns)`.

**New type** (`pkg/runner/limiterror.go`):

```go
type LimitError struct {
    Pattern string  // matched substring
    Tool    string  // executor name, e.g. "codex"
    HelpCmd string  // e.g. "codex /status"
}
func (e *LimitError) Error() string {
    return fmt.Sprintf("rate limit hit on %s: matched %q — %s",
        e.Tool, e.Pattern, e.HelpCmd)
}
```

**`RunRequest` change:**

```go
// REMOVED: RateLimitMaxRetries int
// Detection/classification is executor-owned now.
```

**New helper:**

```go
func DetectLimit(stderrPath string, patterns []string) (matched string, ok bool)
```

Case-insensitive substring scan; returns first match.

**Executor pattern:**

```go
resp, err := runner.Run(ctx, req)
if err != nil {
    if matched, ok := runner.DetectLimit(req.StderrFile, myPatterns); ok {
        return executor.Response{Duration: resp.Duration},
            &runner.LimitError{Pattern: matched, Tool: c.Name(),
                HelpCmd: c.helpCmd()}
    }
}
```

**Runner stays oblivious.** `runner.Run` no longer scans stderr for
429; returns `ErrNonZeroExit`; executors classify further.
`ratelimit.go`'s `scanStderr` is removed; `DetectLimit` replaces it.

**Orchestrator on `*LimitError`:**

- Treated as failed survivor for quorum (same path as timeout /
  non-zero exit).
- Collected into per-round `rateLimits []*LimitError`.
- Serialized into `verdict.json` as optional top-level `rate_limits`
  array: `[{"executor":"codex","pattern":"...","help_cmd":"codex /status",
  "round":1,"expert":"codex_expert"}]`. Additive; no schema bump.
- If `survivors < quorum && len(rateLimits) > 0`, `debate.Run*`
  returns `ErrRateLimitQuorumFail`.

**`cmd/council` on `ErrRateLimitQuorumFail`:**

- Exit code 6 (`ExitRateLimitQuorumFail`). Distinct from 2 (generic
  quorum fail).
- Print per-CLI help footer, one line per affected CLI:
  ```
  error: rate limit hit on codex — run 'codex /status' for more information
  error: rate limit hit on gemini-cli — check https://aistudio.google.com/apikey
  error: quorum failed (1/2 experts surviving, 2 rate-limited)
  ```
- verdict.json still written (status `quorum_failed` + `rate_limits`).

## Alternatives considered

**a) Keep retry with lower budget.** Rejected — the number isn't the
problem; the duplication is.

**b) Exit council run on any 429.** Rejected — discards 2-of-3
survivor work that ADR-0012's whole design is meant to preserve.

**c) Session-level retry (re-run the whole session after delay).**
Deferred to v4.

**d) Backwards-compat retry flag.** N/A — single-user project,
no in-flight v2 callers; behavior shift just lands.

## Consequences

- `pkg/runner.Run` shrinks.
- Rate-limit markers + HelpCmd live next to per-CLI invocation
  specs — cohesive.
- `verdict.json` gains additive field; no schema bump.
- New exit code 6.
- Quorum absorbs single-vendor rate-limit (the whole point of
  ADR-0012); two-vendor rate-limit fails the run with exit 6.

## Fitness functions

| # | Fitness | Check |
|---|---|---|
| F31 | No retry on first rate-limit hit | Stub CLI emits marker on first attempt → executor returns `*LimitError` with no delay |
| F32 | LimitError fields populated | Tool + Pattern + HelpCmd non-empty per-CLI |
| F33 | Quorum absorbs single rate-limit | 3-expert profile, one mocked → `*LimitError`; quorum=2; verdict succeeds; `rate_limits` populated |
| F34 | Quorum fails on ≥2 rate-limits → exit 6 | 3-expert, two rate-limited → exit 6; stderr shows two help footers |
| F35 | verdict.json.rate_limits shape | Rate-limited expert produces expected entry |

## Status

Proposed. Paired with ADR-0012.

