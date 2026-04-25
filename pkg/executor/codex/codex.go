// Package codex is the v3 Executor backed by OpenAI's `codex` CLI
// ("codex-cli"). Importing this package for side effects
// (`_ "github.com/fitz123/council/pkg/executor/codex"`) registers the
// executor under the name "codex" via init().
//
// Translation contract (ADR-0012):
//   - AllowedTools containing any web tool (WebSearch / WebFetch) →
//     argv gets `-c tools.web_search=true` (codex's hosted web_search
//     covers fetch semantics; one flag handles both).
//   - PermissionMode is irrelevant for codex — `--sandbox read-only`
//     plus `--ephemeral` already cover the auto-approval surface.
//
// Rate-limit detection lives here per ADR-0013: on a runner error we
// scan the captured stderr against codexLimitPatterns and, on match,
// wrap the runner error into *runner.LimitError so pkg/debate can
// classify it via errors.As.
package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/runner"
)

// codexLimitPatterns is the substring list DetectLimit scans against on
// a non-zero exit. Sourced from `codex-rs/protocol/src/error.rs` per
// ADR-0012; case-insensitive matching is performed by DetectLimit. If
// a future codex release surfaces an un-matched 429, add the substring
// here as a one-liner — strict-YAGNI per project convention, no regex
// fallback.
var codexLimitPatterns = []string{
	"you've hit your usage limit",
	"quota exceeded. check your plan",
	"selected model is at capacity",
	"exceeded retry limit, last status: 429",
	"upgrade to plus to continue",
}

// codexHelpCmd is what we tell the user to run when codex is rate-
// limited. Surfaced in the per-CLI footer cmd/council/main.go prints
// on ExitRateLimitQuorumFail.
const codexHelpCmd = "codex /status"

// Codex wraps the `codex` CLI. Binary defaults to "codex"; tests
// override it with a stub-script path. The override has no effect on
// BinaryName() — preflight always looks up the canonical name.
type Codex struct {
	Binary string
}

// init registers a default-configured instance as "codex".
func init() {
	executor.Register(&Codex{})
}

// Name returns the registry key used in profile YAML.
func (c *Codex) Name() string { return "codex" }

// BinaryName returns the program name to look up on PATH for preflight.
// Constant "codex" — c.Binary is a test-only override.
func (c *Codex) BinaryName() string { return "codex" }

// MapModel is identity. Codex model IDs (gpt-5.5, etc.) pass through
// to `codex exec -m <id>` verbatim.
func (c *Codex) MapModel(model string) string { return model }

func (c *Codex) binary() string {
	if c.Binary == "" {
		return "codex"
	}
	return c.Binary
}

// Execute runs one `codex exec` invocation per ADR-0012. Returns a
// non-nil error on empty Model, missing stdio paths, non-positive
// timeout, or any error from pkg/runner. On non-zero exit the captured
// stderr is scanned for codexLimitPatterns; on match the error is
// wrapped into *runner.LimitError.
func (c *Codex) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	if req.Model == "" {
		return executor.Response{}, errors.New("codex: empty Model")
	}
	if req.StdoutFile == "" || req.StderrFile == "" {
		return executor.Response{}, errors.New("codex: StdoutFile and StderrFile required")
	}
	if req.Timeout <= 0 {
		return executor.Response{}, fmt.Errorf("codex: Timeout must be positive, got %s", req.Timeout)
	}

	argv := []string{
		c.binary(), "exec",
		"-m", c.MapModel(req.Model),
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ephemeral",
		"--color", "never",
	}
	// Translation: any web tool in AllowedTools enables codex's hosted
	// web_search once. The hosted tool covers fetch semantics, so a
	// lone WebFetch entry triggers the same single flag.
	if containsWebTool(req.AllowedTools) {
		argv = append(argv, "-c", "tools.web_search=true")
	}
	// Trailing `-` makes codex read the prompt from stdin.
	argv = append(argv, "-")

	resp, err := runner.Run(ctx, runner.RunRequest{
		Argv:       argv,
		Prompt:     req.Prompt,
		StdoutFile: req.StdoutFile,
		StderrFile: req.StderrFile,
		Timeout:    req.Timeout,
		// MaxRetries=0: orchestrator owns fail-retry; rate-limit
		// failures are wrapped into *LimitError below and absorbed by
		// quorum (ADR-0013).
		MaxRetries: 0,
	})

	out := executor.Response{
		ExitCode: resp.ExitCode,
		Duration: durationOrZero(resp.Duration),
	}
	if err != nil {
		if pat, ok := runner.DetectLimit(req.StderrFile, codexLimitPatterns); ok {
			return out, &runner.LimitError{Pattern: pat, Tool: c.Name(), HelpCmd: codexHelpCmd}
		}
		return out, err
	}
	return out, nil
}

// containsWebTool reports whether tools mentions any web-tool name the
// debate engine grants experts. Match is case-insensitive against the
// two canonical names (WebSearch, WebFetch) — both map to codex's
// single hosted web_search tool, so one match is enough.
func containsWebTool(tools []string) bool {
	for _, t := range tools {
		switch strings.ToLower(t) {
		case "websearch", "webfetch":
			return true
		}
	}
	return false
}

func durationOrZero(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
