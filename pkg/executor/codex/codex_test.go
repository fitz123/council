package codex

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

// writeStub drops a POSIX-shell stub at t.TempDir/stub-codex.sh that
// echoes its argv and exits 0. The argv goes to the StdoutFile so a
// single os.ReadFile lets the test assert argv shape.
func writeStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-codex.sh")
	body := `#!/bin/sh
echo "ARGV: $*"
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// readArgvTokens reads the stub's stdout and returns the tokens after
// "ARGV: ". Splitting on whitespace is sufficient because none of the
// argv entries codex passes contain spaces.
func readArgvTokens(t *testing.T, stdoutPath string) []string {
	t.Helper()
	data, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "ARGV: ") {
			return strings.Fields(strings.TrimPrefix(line, "ARGV: "))
		}
	}
	t.Fatalf("ARGV line not found in stdout: %q", string(data))
	return nil
}

func TestNameAndBinaryName(t *testing.T) {
	c := &Codex{}
	if got := c.Name(); got != "codex" {
		t.Errorf("Name() = %q, want codex", got)
	}
	if got := c.BinaryName(); got != "codex" {
		t.Errorf("BinaryName() = %q, want codex", got)
	}
	// Test override does not affect BinaryName — preflight contract is
	// the canonical binary name, not the test stub path.
	cWithOverride := &Codex{Binary: "/tmp/stub"}
	if got := cWithOverride.BinaryName(); got != "codex" {
		t.Errorf("BinaryName() with override = %q, want codex", got)
	}
}

func TestMapModelIdentity(t *testing.T) {
	c := &Codex{}
	for _, m := range []string{"gpt-5.5", "gpt-5", "o1-preview", "future-model"} {
		if got := c.MapModel(m); got != m {
			t.Errorf("MapModel(%q) = %q, want identity", m, got)
		}
	}
}

func TestExecuteRejectsEmptyModel(t *testing.T) {
	c := &Codex{Binary: writeStub(t)}
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

// TestExecuteHappyPathNoTools verifies the canonical argv shape with
// no AllowedTools — must NOT contain `-c tools.web_search=true`.
func TestExecuteHappyPathNoTools(t *testing.T) {
	stub := writeStub(t)
	c := &Codex{Binary: stub}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")

	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "the question\n",
		Model:      "gpt-5.5",
		Timeout:    5 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tokens := readArgvTokens(t, out)
	want := []string{
		"exec",
		"-m", "gpt-5.5",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ephemeral",
		"--color", "never",
		"-",
	}
	if len(tokens) != len(want) {
		t.Fatalf("argv length = %d, want %d\ngot:  %v\nwant: %v", len(tokens), len(want), tokens, want)
	}
	for i, w := range want {
		if tokens[i] != w {
			t.Errorf("argv[%d] = %q, want %q\nfull argv: %v", i, tokens[i], w, tokens)
		}
	}
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "tools.web_search") {
			t.Errorf("argv unexpectedly contains web_search token: %v", tokens)
		}
	}
}

// TestExecuteEmitsWebSearchFlag locks in the two-token form
// `-c tools.web_search=true` (NOT the concatenated `-c=...` shape).
// Asserts the consecutive-position requirement from the plan.
func TestExecuteEmitsWebSearchFlag(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
	}{
		{name: "WebSearch+WebFetch", tools: []string{"WebSearch", "WebFetch"}},
		{name: "WebSearch alone", tools: []string{"WebSearch"}},
		{name: "WebFetch alone", tools: []string{"WebFetch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := writeStub(t)
			c := &Codex{Binary: stub}

			tmp := t.TempDir()
			out := filepath.Join(tmp, "out")
			errf := filepath.Join(tmp, "err")

			_, err := c.Execute(context.Background(), executor.Request{
				Prompt:       "x",
				Model:        "gpt-5.5",
				Timeout:      5 * time.Second,
				StdoutFile:   out,
				StderrFile:   errf,
				AllowedTools: tc.tools,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			tokens := readArgvTokens(t, out)
			// Find a two-consecutive `-c` followed by exactly
			// `tools.web_search=true`. Also assert the concatenated
			// `-c=tools.web_search=true` form does NOT appear.
			found := false
			for i := 0; i < len(tokens)-1; i++ {
				if tokens[i] == "-c" && tokens[i+1] == "tools.web_search=true" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("argv missing consecutive `-c tools.web_search=true`\ngot: %v", tokens)
			}
			for _, tok := range tokens {
				if tok == "-c=tools.web_search=true" {
					t.Errorf("argv uses concatenated form `-c=tools.web_search=true`; want two-token form\ngot: %v", tokens)
				}
			}
			// Trailing `-` must still close out the argv (stdin
			// sentinel) so codex reads the prompt from stdin.
			if tokens[len(tokens)-1] != "-" {
				t.Errorf("argv last token = %q, want `-` (stdin sentinel)\nfull argv: %v", tokens[len(tokens)-1], tokens)
			}
		})
	}
}

// TestExecuteTimeoutPropagated verifies that a slow stub is killed by
// the runner's timeout and the resulting error is surfaced.
func TestExecuteTimeoutPropagated(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "slow.sh")
	body := `#!/bin/sh
sleep 10
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Codex{Binary: stub}

	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "gpt-5.5",
		Timeout:    100 * time.Millisecond,
		StdoutFile: filepath.Join(dir, "out"),
		StderrFile: filepath.Join(dir, "err"),
	})
	if err == nil {
		t.Fatal("Execute on slow stub: want timeout error, got nil")
	}
	if !errors.Is(err, runner.ErrTimeout) {
		t.Errorf("err = %v, want runner.ErrTimeout", err)
	}
}

// TestExecutePropagatesNonZeroExit verifies a generic non-zero exit
// (no rate-limit marker in stderr) is NOT classified as *LimitError.
func TestExecutePropagatesNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "fail.sh")
	body := `#!/bin/sh
echo "generic error" >&2
exit 17
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Codex{Binary: stub}

	resp, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "gpt-5.5",
		Timeout:    5 * time.Second,
		StdoutFile: filepath.Join(dir, "out"),
		StderrFile: filepath.Join(dir, "err"),
	})
	if err == nil {
		t.Fatal("Execute on failing stub: want error, got nil")
	}
	if resp.ExitCode != 17 {
		t.Errorf("ExitCode = %d, want 17", resp.ExitCode)
	}
	var le *runner.LimitError
	if errors.As(err, &le) {
		t.Errorf("non-rate-limit exit classified as LimitError: %v", err)
	}
}

// TestExecuteWrapsRateLimitMarkers covers the 5 codex rate-limit
// patterns. Each marker case asserts the executor wraps into
// *runner.LimitError{Tool:"codex", Pattern:<matched>, HelpCmd:"codex /status"}.
func TestExecuteWrapsRateLimitMarkers(t *testing.T) {
	for _, pattern := range codexLimitPatterns {
		t.Run(pattern, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "stub.sh")
			markerPath := filepath.Join(dir, "marker.txt")
			if err := os.WriteFile(markerPath, []byte(pattern+"\n"), 0o644); err != nil {
				t.Fatalf("write marker: %v", err)
			}
			body := "#!/bin/sh\ncat " + markerPath + " >&2\nexit 1\n"
			if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
				t.Fatalf("write stub: %v", err)
			}
			c := &Codex{Binary: stub}

			_, err := c.Execute(context.Background(), executor.Request{
				Prompt:     "x",
				Model:      "gpt-5.5",
				Timeout:    5 * time.Second,
				StdoutFile: filepath.Join(dir, "out"),
				StderrFile: filepath.Join(dir, "err"),
			})
			var le *runner.LimitError
			if !errors.As(err, &le) {
				t.Fatalf("err = %v, want *runner.LimitError", err)
			}
			if le.Tool != "codex" {
				t.Errorf("LimitError.Tool = %q, want codex", le.Tool)
			}
			if le.HelpCmd != codexHelpCmd {
				t.Errorf("LimitError.HelpCmd = %q, want %q", le.HelpCmd, codexHelpCmd)
			}
			if le.Pattern != pattern {
				t.Errorf("LimitError.Pattern = %q, want %q", le.Pattern, pattern)
			}
		})
	}
}

func TestInitRegistersCodex(t *testing.T) {
	got, err := executor.Get("codex")
	if err != nil {
		t.Fatalf("Get(codex) after import: %v", err)
	}
	if got.Name() != "codex" {
		t.Errorf("registered Name() = %q, want codex", got.Name())
	}
}
