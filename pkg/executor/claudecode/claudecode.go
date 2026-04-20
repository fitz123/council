// Package claudecode is the v1 Executor backed by Anthropic's `claude`
// CLI ("Claude Code"). Importing this package for side effects
// (`_ "github.com/fitz123/council/pkg/executor/claudecode"`) registers
// the executor under the name "claude-code" via init().
//
// The executor's job is small on purpose: build the right argv + env,
// hand off to pkg/runner, translate the result to executor.Response.
// All the hard subprocess logic — process-group kill, timeout,
// rate-limit retries, stderr-on-failure-only — lives in pkg/runner.
//
// Retry split (see runner package doc): we pass MaxRetries=0 to disable
// runner-side fail-retry. The orchestrator owns fail-retry policy via
// the profile's max_retries field. Rate-limit retries are unaffected by
// this — those stay runner-owned.
package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/runner"
)

// envMaxOutputTokens is the only environment variable claude-code needs
// from us. 64000 matches design/v1.md §7. Centralized here so a future
// bump (or a per-call override) has one obvious place to land.
const envMaxOutputTokens = "CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000"

// ClaudeCode wraps the `claude` CLI. Binary defaults to "claude", but
// is exported so tests can point it at a stub script. Real callers
// should leave it zero and let the default apply.
type ClaudeCode struct {
	// Binary is the program to exec. Empty means "claude" (resolved via
	// PATH at exec time). Tests substitute a path to a stub.
	Binary string
}

// init registers a default-configured instance as "claude-code". Side-
// effect imports get the registration "for free".
func init() {
	executor.Register(&ClaudeCode{})
}

// Name returns the registry key. Stable user-visible string — changing
// it is a breaking change to every existing profile YAML.
func (c *ClaudeCode) Name() string { return "claude-code" }

// MapModel translates the vendor-agnostic short name from profile YAML
// to the value the CLI's --model flag expects. v1 mapping is identity
// (haiku/sonnet/opus pass through unchanged); it lives on the type
// rather than as a free function so v2 can override it on a per-instance
// basis if a model release ever needs translation.
func (c *ClaudeCode) MapModel(model string) string { return model }

// binary returns the program name to exec, defaulting to "claude".
func (c *ClaudeCode) binary() string {
	if c.Binary == "" {
		return "claude"
	}
	return c.Binary
}

// Execute runs one `claude -p` invocation per design/v1.md §7.
//
// It returns a non-nil error on:
//   - empty Model (programming error — profile validator should catch
//     this earlier; we double-check rather than letting an empty --model
//     reach the CLI)
//   - any error from pkg/runner (timeout, non-zero exit, or
//     unrecoverable rate-limit)
//
// On error the partial Response (with Duration set) is still returned
// so the orchestrator can populate verdict.json with whatever did
// happen.
func (c *ClaudeCode) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	if req.Model == "" {
		return executor.Response{}, errors.New("claudecode: empty Model")
	}
	if req.StdoutFile == "" || req.StderrFile == "" {
		return executor.Response{}, errors.New("claudecode: StdoutFile and StderrFile required")
	}
	if req.Timeout <= 0 {
		return executor.Response{}, fmt.Errorf("claudecode: Timeout must be positive, got %s", req.Timeout)
	}

	argv := []string{
		c.binary(),
		"-p", "-",
		"--model", c.MapModel(req.Model),
		"--output-format", "text",
	}

	// Inherit the parent environment so PATH, HOME, ANTHROPIC_API_KEY,
	// etc. propagate, then append our token-budget knob. nil Env in
	// pkg/runner means "inherit"; passing an explicit slice replaces, so
	// we must materialize os.Environ ourselves.
	env := append(os.Environ(), envMaxOutputTokens)

	resp, err := runner.Run(ctx, runner.RunRequest{
		Argv:       argv,
		Prompt:     req.Prompt,
		Env:        env,
		StdoutFile: req.StdoutFile,
		StderrFile: req.StderrFile,
		Timeout:    req.Timeout,
		// MaxRetries=0 — orchestrator owns fail-retry policy. Rate-limit
		// retries still happen inside pkg/runner; the budget is
		// req.MaxRetries+1 per design/v1.md §10 ("retry up to
		// max_retries + 1 times").
		MaxRetries:          0,
		RateLimitMaxRetries: req.MaxRetries + 1,
	})

	out := executor.Response{
		ExitCode: resp.ExitCode,
		Duration: durationOrZero(resp.Duration),
	}
	return out, err
}

// durationOrZero rounds nothing — kept as a one-line helper so a future
// version can clamp negative / NaN-equivalent durations if pkg/runner's
// contract ever loosens. Today it is a passthrough.
func durationOrZero(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
