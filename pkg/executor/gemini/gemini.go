// Package gemini is the v3 Executor backed by Google's `gemini` CLI
// ("gemini-cli"). Importing this package for side effects
// (`_ "github.com/fitz123/council/pkg/executor/gemini"`) registers the
// executor under the name "gemini-cli" via init().
//
// Translation contract (ADR-0012):
//   - PermissionMode == "bypassPermissions" → executor writes the
//     embedded geminiPolicyTOML to a fresh ephemeral tmp dir and
//     appends `--policy <that path>` to argv. Empty PermissionMode →
//     no policy file, no `--policy` flag (gemini's default-deny on
//     web_fetch in headless applies; ballot invariant).
//   - AllowedTools is irrelevant for gemini — the policy TOML allows
//     google_web_search + web_fetch when bypassPermissions is set, so
//     the per-tool list does not need separate translation.
//
// We do NOT set $GEMINI_CLI_HOME: gemini-cli treats GEMINI_CLI_HOME as
// the parent of `.gemini/` (where OAuth creds live), so redirecting it
// to a fresh dir would mask the user's `~/.gemini/oauth_creds.json` and
// fail every call with "Please set an Auth method". gemini's headless
// `-p` mode is already stateless enough for our purposes; only the
// policy file needs an ephemeral home.
//
// `--yolo` and `--allowed-tools` are deprecated in gemini-cli 0.38.2
// and slated for removal in 1.0; the Policy Engine (`--policy <file>`)
// is the forward-compatible surface — see ADR-0012 alternatives g/h/h'.
//
// Rate-limit detection lives here per ADR-0013: on a runner error we
// scan the captured stderr against geminiLimitPatterns and, on match,
// wrap the runner error into *runner.LimitError so pkg/debate can
// classify it via errors.As.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/runner"
)

// geminiPolicyTOML is the body written to <ephemeral>/policy.toml
// when PermissionMode == bypassPermissions. Allows the two web tools
// gemini's headless mode would otherwise default-deny. Schema source:
// gemini-cli docs/core/policy-engine. Stable enough to embed inline;
// a schema bump is a one-line edit per ADR-0012 R-v3-08.
const geminiPolicyTOML = "[[rule]]\ntoolName = [\"google_web_search\", \"web_fetch\"]\ndecision = \"allow\"\npriority = 100\n"

// geminiLimitPatterns is the substring list DetectLimit scans against
// on a non-zero exit. Sourced from
// `packages/core/src/utils/googleQuotaErrors.ts` per ADR-0012;
// case-insensitive matching is performed by DetectLimit. If a future
// gemini release surfaces an un-matched 429, add the substring here as
// a one-liner — strict-YAGNI per project convention, no regex
// fallback.
var geminiLimitPatterns = []string{
	"RESOURCE_EXHAUSTED",
	"QUOTA_EXHAUSTED",
	"RATE_LIMIT_EXCEEDED",
	"exceeded your current quota",
	"Please retry in",
}

// geminiHelpCmd is what we tell the user to run when gemini is rate-
// limited. Surfaced in the per-CLI footer cmd/council/main.go prints
// on ExitRateLimitQuorumFail.
const geminiHelpCmd = "check https://aistudio.google.com/apikey for quota and billing"

// Gemini wraps the `gemini` CLI. Binary defaults to "gemini"; tests
// override it with a stub-script path. The override has no effect on
// BinaryName() — preflight always looks up the canonical name.
type Gemini struct {
	Binary string
}

// init registers a default-configured instance as "gemini-cli".
func init() {
	executor.Register(&Gemini{})
}

// Name returns the registry key used in profile YAML.
func (g *Gemini) Name() string { return "gemini-cli" }

// BinaryName returns the program name to look up on PATH for preflight.
// Constant "gemini" — g.Binary is a test-only override.
func (g *Gemini) BinaryName() string { return "gemini" }

// MapModel is identity. Gemini model IDs (gemini-3.1-pro-preview, etc.)
// pass through to `gemini -m <id>` verbatim.
func (g *Gemini) MapModel(model string) string { return model }

func (g *Gemini) binary() string {
	if g.Binary == "" {
		return "gemini"
	}
	return g.Binary
}

// Execute runs one `gemini` invocation per ADR-0012. Returns a non-nil
// error on empty Model, missing stdio paths, non-positive timeout, or
// any error from pkg/runner. On non-zero exit the captured stderr is
// scanned for geminiLimitPatterns; on match the error is wrapped into
// *runner.LimitError.
//
// When PermissionMode == bypassPermissions a fresh tmp dir is created
// per call, policy.toml is written into it, and the dir is removed on
// return. RemoveAll fires after runner.Run blocks on cmd.Wait, so the
// subprocess has finished reading the policy by the time we tear it
// down. The user's `~/.gemini/` (OAuth creds, settings) is not touched.
func (g *Gemini) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	if req.Model == "" {
		return executor.Response{}, errors.New("gemini: empty Model")
	}
	if req.StdoutFile == "" || req.StderrFile == "" {
		return executor.Response{}, errors.New("gemini: StdoutFile and StderrFile required")
	}
	if req.Timeout <= 0 {
		return executor.Response{}, fmt.Errorf("gemini: Timeout must be positive, got %s", req.Timeout)
	}

	argv := []string{
		g.binary(),
		"-m", g.MapModel(req.Model),
		"-o", "text",
	}
	// Translation: bypassPermissions is the webfetch expert-path
	// signal. Write the embedded policy TOML to a fresh tmp dir and
	// pass --policy. Empty PermissionMode emits nothing; gemini's
	// default-deny on web_fetch in headless applies (ballot
	// invariant).
	if req.PermissionMode == "bypassPermissions" {
		tmpDir, err := os.MkdirTemp("", "gemini-policy-*")
		if err != nil {
			return executor.Response{}, fmt.Errorf("gemini: mkdir temp: %w", err)
		}
		defer os.RemoveAll(tmpDir)
		policyPath := filepath.Join(tmpDir, "policy.toml")
		if writeErr := os.WriteFile(policyPath, []byte(geminiPolicyTOML), 0o600); writeErr != nil {
			return executor.Response{}, fmt.Errorf("gemini: write policy: %w", writeErr)
		}
		argv = append(argv, "--policy", policyPath)
	}

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
		if pat, ok := runner.DetectLimit(req.StderrFile, geminiLimitPatterns); ok {
			return out, &runner.LimitError{Pattern: pat, Tool: g.Name(), HelpCmd: geminiHelpCmd}
		}
		return out, err
	}
	return out, nil
}

func durationOrZero(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
