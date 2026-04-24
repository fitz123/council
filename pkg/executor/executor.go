// Package executor is the CLI plugin contract. The orchestrator never
// invokes a vendor CLI directly; it goes through the Executor interface,
// which is the sole extension point for adding new CLIs (Codex, Gemini,
// etc.) in v2 without touching orchestrator code.
//
// MVP ships one implementation, pkg/executor/claudecode, which wraps
// `claude -p`. Adding a new executor in v2 is: new file in a new
// subpackage, register it from the subpackage's init(), add an
// `executor: <name>` value to a profile YAML — no orchestrator change.
//
// The interface, Request, and Response shapes are bytes-exact with
// docs/design/v1.md §7. Do not add fields without updating §7 and the
// matching ADR.
package executor

import (
	"context"
	"time"
)

// Executor is implemented by every backend CLI. Name is the stable token
// users put in profile YAML (`executor: claude-code`). Execute spawns one
// invocation and writes its stdout/stderr to the files named in req; it
// must not retry, must not delete the stderr file, and must not emit any
// log of its own — those concerns belong to pkg/runner and
// pkg/orchestrator respectively. Implementations are expected to delegate
// to pkg/runner.Run for the actual subprocess work.
type Executor interface {
	Name() string
	Execute(ctx context.Context, req Request) (Response, error)
}

// Request describes one Execute call. Field-for-field with design §7.
//
// Prompt is piped to the subprocess on stdin in full. Model is the
// vendor-agnostic short name ("haiku" / "sonnet" / "opus"); each
// Executor implementation maps it to the CLI-side flag value (see
// ClaudeCode.MapModel for the v1 mapping, which is identity).
//
// MaxRetries is the profile's max_retries value, passed through so the
// executor can size its rate-limit retry budget per design/v1.md §10's
// `max_retries + 1` rule. Fail-retry policy stays orchestrator-owned.
//
// AllowedTools and PermissionMode are the ADR-0010 hooks the v2 debate
// engine uses to grant experts WebSearch/WebFetch during R1 and R2.
// Empty values preserve v1 behavior for any caller that does not set
// them — no `--allowedTools` / `--permission-mode` flag is emitted by
// the claude-code executor in that case. Ballot subprocesses
// hard-code both fields to zero (`AllowedTools: nil`, `PermissionMode:
// ""`) so voting is always tools-off regardless of expert defaults.
type Request struct {
	Prompt         string
	Model          string
	Timeout        time.Duration
	StdoutFile     string
	StderrFile     string
	MaxRetries     int
	AllowedTools   []string
	PermissionMode string
}

// Response is what Execute returns on a non-error completion. ExitCode
// is the subprocess's exit status (always 0 for the success path; the
// non-zero path returns an error from pkg/runner). Duration is wall-clock
// time including any rate-limit retries swallowed inside pkg/runner.
type Response struct {
	ExitCode int
	Duration time.Duration
}
