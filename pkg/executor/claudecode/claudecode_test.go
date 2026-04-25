package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/runner"
)

// writeStub drops a small POSIX-shell script in t.TempDir that:
//   - echoes its argv
//   - echoes the env var the executor is supposed to set
//   - copies stdin verbatim
//
// All three signals land in the StdoutFile, so a single os.ReadFile
// suffices to verify argv shape, env injection, and stdin piping. The
// script is chmod'd 0o755 and its absolute path is returned for
// ClaudeCode.Binary.
func writeStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-claude.sh")
	body := `#!/bin/sh
echo "ARGV: $*"
echo "TOKENS: ${CLAUDE_CODE_MAX_OUTPUT_TOKENS:-unset}"
echo "PARENT_ENV: ${COUNCIL_TEST_PARENT_ENV:-unset}"
echo "STDIN_BEGIN"
cat
echo
echo "STDIN_END"
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestExecuteHappyPath(t *testing.T) {
	// Set a sentinel parent-env var before the Executor runs. If the
	// implementation ever regresses to NOT using os.Environ (e.g. by
	// replacing the env with only CLAUDE_CODE_MAX_OUTPUT_TOKENS), the
	// sentinel won't reach the subprocess and the assertion below fails.
	t.Setenv("COUNCIL_TEST_PARENT_ENV", "sentinel-value")

	stub := writeStub(t)
	c := &ClaudeCode{Binary: stub}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")
	prompt := "the question body\n"

	resp, err := c.Execute(context.Background(), executor.Request{
		Prompt:     prompt,
		Model:      "sonnet",
		Timeout:    5 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", resp.ExitCode)
	}
	if resp.Duration <= 0 {
		t.Errorf("Duration = %s, want > 0", resp.Duration)
	}

	got, _ := os.ReadFile(out)
	gotStr := string(got)

	// argv assertion: must contain the four flag tokens in the right
	// order. We assert on a substring rather than the full line so
	// future additions (a verbose flag, etc.) don't break the test.
	wantArgv := "-p - --model sonnet --output-format text"
	if !strings.Contains(gotStr, "ARGV: "+wantArgv) {
		t.Errorf("argv mismatch.\ngot:  %q\nwant substring: %q", gotStr, wantArgv)
	}

	// env assertion: the executor must set CLAUDE_CODE_MAX_OUTPUT_TOKENS
	// to 64000 per design §7.
	if !strings.Contains(gotStr, "TOKENS: 64000") {
		t.Errorf("env not propagated: stdout = %q", gotStr)
	}

	// parent-env assertion: the executor appends its token-budget knob
	// to os.Environ rather than replacing it, so PATH / HOME /
	// ANTHROPIC_API_KEY / COUNCIL_TEST_PARENT_ENV all reach the child.
	if !strings.Contains(gotStr, "PARENT_ENV: sentinel-value") {
		t.Errorf("parent env not inherited: stdout = %q", gotStr)
	}

	// stdin assertion: prompt body must reach the subprocess verbatim.
	wantPiped := "STDIN_BEGIN\n" + prompt + "\nSTDIN_END\n"
	if !strings.Contains(gotStr, wantPiped) {
		t.Errorf("stdin not piped.\ngot:  %q\nwant substring: %q", gotStr, wantPiped)
	}
}

func TestExecuteOverridesInheritedTokenBudget(t *testing.T) {
	// If the caller exported CLAUDE_CODE_MAX_OUTPUT_TOKENS before running
	// council, the child must still see 64000 (README §Environment).
	// Without the dedup, both entries reach execve and glibc's getenv
	// returns the first match — i.e. the caller's value would win.
	t.Setenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS", "12345")

	stub := writeStub(t)
	c := &ClaudeCode{Binary: stub}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")

	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "sonnet",
		Timeout:    5 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(out)
	gotStr := string(got)
	if !strings.Contains(gotStr, "TOKENS: 64000") {
		t.Errorf("token budget not overridden: stdout = %q", gotStr)
	}
	if strings.Contains(gotStr, "TOKENS: 12345") {
		t.Errorf("inherited token budget leaked to child: stdout = %q", gotStr)
	}
}

func TestExecuteModelMappingIsIdentity(t *testing.T) {
	// v1 contract: MapModel is identity. Test the three documented
	// names plus a hypothetical future one to lock the behavior in.
	c := &ClaudeCode{}
	for _, m := range []string{"haiku", "sonnet", "opus", "sonnet-future"} {
		if got := c.MapModel(m); got != m {
			t.Errorf("MapModel(%q) = %q, want identity", m, got)
		}
	}
}

func TestExecuteRejectsEmptyModel(t *testing.T) {
	c := &ClaudeCode{Binary: writeStub(t)}
	tmp := t.TempDir()
	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "",
		Timeout:    time.Second,
		StdoutFile: filepath.Join(tmp, "out"),
		StderrFile: filepath.Join(tmp, "err"),
	})
	if err == nil {
		t.Fatal("Execute with empty Model: want error, got nil")
	}
}

func TestExecuteRejectsZeroTimeout(t *testing.T) {
	c := &ClaudeCode{Binary: writeStub(t)}
	tmp := t.TempDir()
	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "sonnet",
		Timeout:    0,
		StdoutFile: filepath.Join(tmp, "out"),
		StderrFile: filepath.Join(tmp, "err"),
	})
	if err == nil {
		t.Fatal("Execute with zero Timeout: want error, got nil")
	}
}

func TestExecuteRejectsMissingFiles(t *testing.T) {
	c := &ClaudeCode{Binary: writeStub(t)}
	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:  "x",
		Model:   "sonnet",
		Timeout: time.Second,
		// StdoutFile/StderrFile both empty
	})
	if err == nil {
		t.Fatal("Execute with empty stdio paths: want error, got nil")
	}
}

func TestNameIsStable(t *testing.T) {
	// design/v1.md §7 names this "claude-code". Drift is a breaking
	// change to every existing profile YAML — assert it explicitly.
	c := &ClaudeCode{}
	if c.Name() != "claude-code" {
		t.Errorf("Name() = %q, want claude-code", c.Name())
	}
}

func TestBinaryNameIsClaude(t *testing.T) {
	// Preflight (cmd/council/preflight.go) does exec.LookPath against
	// BinaryName(). The registry key (`claude-code`) is not a valid
	// binary name — the actual executable is `claude`. The test-only
	// Binary override on the struct is irrelevant to the preflight
	// contract: BinaryName must always return "claude".
	c := &ClaudeCode{}
	if got := c.BinaryName(); got != "claude" {
		t.Errorf("BinaryName() = %q, want claude", got)
	}
	cWithOverride := &ClaudeCode{Binary: "/tmp/stub"}
	if got := cWithOverride.BinaryName(); got != "claude" {
		t.Errorf("BinaryName() with override Binary = %q, want claude", got)
	}
}

func TestInitRegistersClaudeCode(t *testing.T) {
	// importing this package must register "claude-code" via init().
	got, err := executor.Get("claude-code")
	if err != nil {
		t.Fatalf("Get(claude-code) after import: %v", err)
	}
	if got.Name() != "claude-code" {
		t.Errorf("registered Name() = %q, want claude-code", got.Name())
	}
}

// TestExecuteEmitsToolFlagsConditionally locks in the ADR-0010 D17
// flag shape: --allowedTools is a comma-joined CSV of tool names,
// --permission-mode is the mode string, and BOTH flags are omitted
// entirely when the corresponding Request field is empty. The empty-
// field case is the v1-compatibility guarantee documented on
// executor.Request — any caller that does not opt in sees zero argv
// delta.
func TestExecuteEmitsToolFlagsConditionally(t *testing.T) {
	cases := []struct {
		name           string
		allowedTools   []string
		permissionMode string
		wantContains   []string
		wantAbsent     []string
	}{
		{
			name:           "empty preserves v1 argv",
			allowedTools:   nil,
			permissionMode: "",
			wantContains:   []string{"-p - --model sonnet --output-format text"},
			wantAbsent:     []string{"--allowedTools", "--permission-mode"},
		},
		{
			name:           "both fields set emits both flags",
			allowedTools:   []string{"WebSearch", "WebFetch"},
			permissionMode: "bypassPermissions",
			wantContains: []string{
				"--allowedTools WebSearch,WebFetch",
				"--permission-mode bypassPermissions",
			},
		},
		{
			name:           "single tool yields single-entry csv",
			allowedTools:   []string{"WebFetch"},
			permissionMode: "bypassPermissions",
			wantContains: []string{
				"--allowedTools WebFetch",
				"--permission-mode bypassPermissions",
			},
			wantAbsent: []string{"WebSearch"},
		},
		{
			name:           "allowedTools only (mode empty) emits only --allowedTools",
			allowedTools:   []string{"WebSearch"},
			permissionMode: "",
			wantContains:   []string{"--allowedTools WebSearch"},
			wantAbsent:     []string{"--permission-mode"},
		},
		{
			name:           "permissionMode only (tools nil) emits only --permission-mode",
			allowedTools:   nil,
			permissionMode: "bypassPermissions",
			wantContains:   []string{"--permission-mode bypassPermissions"},
			wantAbsent:     []string{"--allowedTools"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := writeStub(t)
			c := &ClaudeCode{Binary: stub}

			tmp := t.TempDir()
			out := filepath.Join(tmp, "out")
			errf := filepath.Join(tmp, "err")

			_, err := c.Execute(context.Background(), executor.Request{
				Prompt:         "x",
				Model:          "sonnet",
				Timeout:        5 * time.Second,
				StdoutFile:     out,
				StderrFile:     errf,
				AllowedTools:   tc.allowedTools,
				PermissionMode: tc.permissionMode,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			got, _ := os.ReadFile(out)
			gotStr := string(got)
			for _, sub := range tc.wantContains {
				if !strings.Contains(gotStr, sub) {
					t.Errorf("argv missing %q\ngot: %q", sub, gotStr)
				}
			}
			for _, sub := range tc.wantAbsent {
				if strings.Contains(gotStr, sub) {
					t.Errorf("argv unexpectedly contains %q\ngot: %q", sub, gotStr)
				}
			}
		})
	}
}

func TestExecutePropagatesNonZeroExit(t *testing.T) {
	// stub that exits non-zero — exercises the runner-error pass-through.
	dir := t.TempDir()
	stub := filepath.Join(dir, "fail.sh")
	body := `#!/bin/sh
echo "stub error" >&2
exit 42
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &ClaudeCode{Binary: stub}

	resp, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "sonnet",
		Timeout:    5 * time.Second,
		StdoutFile: filepath.Join(dir, "out"),
		StderrFile: filepath.Join(dir, "err"),
	})
	if err == nil {
		t.Fatal("Execute on failing stub: want error, got nil")
	}
	if resp.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", resp.ExitCode)
	}
	// Generic non-zero exit must NOT be classified as a rate-limit error —
	// the orchestrator routes *LimitError into the absorb-by-quorum path,
	// while everything else stays a normal failure.
	var le *runner.LimitError
	if errors.As(err, &le) {
		t.Errorf("non-rate-limit exit classified as LimitError: %v", err)
	}
}

// TestExecuteWrapsRateLimitMarkers verifies ADR-0013's executor-side
// rate-limit detection: when the captured stderr contains any of the
// claudeLimitPatterns, the executor wraps the runner error into a
// *runner.LimitError with Tool="claude-code", HelpCmd="claude /usage",
// and Pattern set to the matched substring.
func TestExecuteWrapsRateLimitMarkers(t *testing.T) {
	for _, pattern := range claudeLimitPatterns {
		t.Run(pattern, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "stub.sh")
			// Embed the pattern verbatim into the stub's stderr so
			// DetectLimit's case-insensitive substring scan finds it.
			// Write the pattern to a sibling file the script `cat`s to
			// stderr — sidesteps shell-quoting issues for patterns that
			// contain apostrophes ("you've hit your limit").
			markerPath := filepath.Join(dir, "marker.txt")
			if err := os.WriteFile(markerPath, []byte(pattern+"\n"), 0o644); err != nil {
				t.Fatalf("write marker: %v", err)
			}
			body := "#!/bin/sh\ncat " + markerPath + " >&2\nexit 1\n"
			if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
				t.Fatalf("write stub: %v", err)
			}
			c := &ClaudeCode{Binary: stub}

			_, err := c.Execute(context.Background(), executor.Request{
				Prompt:     "x",
				Model:      "sonnet",
				Timeout:    5 * time.Second,
				StdoutFile: filepath.Join(dir, "out"),
				StderrFile: filepath.Join(dir, "err"),
			})
			var le *runner.LimitError
			if !errors.As(err, &le) {
				t.Fatalf("err = %v, want *runner.LimitError", err)
			}
			if le.Tool != "claude-code" {
				t.Errorf("LimitError.Tool = %q, want claude-code", le.Tool)
			}
			if le.HelpCmd != claudeHelpCmd {
				t.Errorf("LimitError.HelpCmd = %q, want %q", le.HelpCmd, claudeHelpCmd)
			}
			if le.Pattern != pattern {
				t.Errorf("LimitError.Pattern = %q, want %q", le.Pattern, pattern)
			}
		})
	}
}
