package gemini

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

// writeStub drops a POSIX-shell stub at t.TempDir/stub-gemini.sh that
// echoes its argv, snapshots the file passed via --policy (if any) to
// $COUNCIL_TEST_POLICY_SNAPSHOT before exiting, and returns 0.
// Snapshotting sidesteps the defer os.RemoveAll race where the parent's
// cleanup fires before the test can stat the original.
func writeStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-gemini.sh")
	body := `#!/bin/sh
echo "ARGV: $*"
policy=""
prev=""
for tok in "$@"; do
  if [ "$prev" = "--policy" ]; then
    policy="$tok"
    break
  fi
  prev="$tok"
done
if [ -n "$COUNCIL_TEST_POLICY_SNAPSHOT" ] && [ -n "$policy" ] && [ -f "$policy" ]; then
  cp "$policy" "$COUNCIL_TEST_POLICY_SNAPSHOT"
fi
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// readArgvTokens returns the whitespace-split tokens after "ARGV: ".
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

// readPolicyPath returns the value following --policy in the recorded
// argv, or "" if the flag is absent.
func readPolicyPath(t *testing.T, stdoutPath string) string {
	t.Helper()
	tokens := readArgvTokens(t, stdoutPath)
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i] == "--policy" {
			return tokens[i+1]
		}
	}
	return ""
}

func TestNameAndBinaryName(t *testing.T) {
	g := &Gemini{}
	if got := g.Name(); got != "gemini-cli" {
		t.Errorf("Name() = %q, want gemini-cli", got)
	}
	if got := g.BinaryName(); got != "gemini" {
		t.Errorf("BinaryName() = %q, want gemini", got)
	}
	// Test override does not affect BinaryName — preflight contract is
	// the canonical binary name, not the test stub path.
	gWithOverride := &Gemini{Binary: "/tmp/stub"}
	if got := gWithOverride.BinaryName(); got != "gemini" {
		t.Errorf("BinaryName() with override = %q, want gemini", got)
	}
}

func TestMapModelIdentity(t *testing.T) {
	g := &Gemini{}
	for _, m := range []string{"gemini-3.1-pro-preview", "gemini-2.5-pro", "gemini-flash", "future-model"} {
		if got := g.MapModel(m); got != m {
			t.Errorf("MapModel(%q) = %q, want identity", m, got)
		}
	}
}

func TestExecuteRejectsEmptyModel(t *testing.T) {
	g := &Gemini{Binary: writeStub(t)}
	tmp := t.TempDir()
	_, err := g.Execute(context.Background(), executor.Request{
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

// TestExecuteHappyPathNoTools verifies argv shape with empty
// PermissionMode — must NOT contain --policy and policy.toml must NOT
// be written.
func TestExecuteHappyPathNoTools(t *testing.T) {
	stub := writeStub(t)
	g := &Gemini{Binary: stub}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")
	snap := filepath.Join(tmp, "policy-snapshot.toml")
	t.Setenv("COUNCIL_TEST_POLICY_SNAPSHOT", snap)

	_, err := g.Execute(context.Background(), executor.Request{
		Prompt:     "the question\n",
		Model:      "gemini-3.1-pro-preview",
		Timeout:    5 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tokens := readArgvTokens(t, out)
	want := []string{
		"-m", "gemini-3.1-pro-preview",
		"-o", "text",
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
		if tok == "--policy" {
			t.Errorf("argv unexpectedly contains --policy with empty PermissionMode: %v", tokens)
		}
		if tok == "--allowed-tools" || tok == "--yolo" {
			t.Errorf("argv contains deprecated flag %q: %v", tok, tokens)
		}
	}
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("policy snapshot exists with empty PermissionMode (stat err: %v)", err)
	}
}

// TestExecuteWritesPolicyOnBypass verifies that when
// PermissionMode=bypassPermissions:
//   - argv contains --policy <path> as two consecutive tokens
//   - policy.toml at that path matches geminiPolicyTOML byte-for-byte
//     (verified via snapshot — defer os.RemoveAll fires before test
//     can stat the original)
func TestExecuteWritesPolicyOnBypass(t *testing.T) {
	stub := writeStub(t)
	g := &Gemini{Binary: stub}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")
	snap := filepath.Join(tmp, "policy-snapshot.toml")
	t.Setenv("COUNCIL_TEST_POLICY_SNAPSHOT", snap)

	_, err := g.Execute(context.Background(), executor.Request{
		Prompt:         "x",
		Model:          "gemini-3.1-pro-preview",
		Timeout:        5 * time.Second,
		StdoutFile:     out,
		StderrFile:     errf,
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	policyPath := readPolicyPath(t, out)
	if policyPath == "" {
		t.Fatalf("argv missing --policy <path>\nargv: %v", readArgvTokens(t, out))
	}
	if filepath.Base(policyPath) != "policy.toml" {
		t.Errorf("--policy basename = %q, want policy.toml", filepath.Base(policyPath))
	}

	// Policy snapshot must equal embedded geminiPolicyTOML byte-for-byte.
	got, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("read policy snapshot: %v", err)
	}
	if string(got) != geminiPolicyTOML {
		t.Errorf("policy.toml body mismatch\ngot:  %q\nwant: %q", string(got), geminiPolicyTOML)
	}
}

// TestExecuteRemovesPolicyDir verifies the ephemeral policy directory
// is gone after Execute returns (defer os.RemoveAll runs).
func TestExecuteRemovesPolicyDir(t *testing.T) {
	stub := writeStub(t)
	g := &Gemini{Binary: stub}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")

	_, err := g.Execute(context.Background(), executor.Request{
		Prompt:         "x",
		Model:          "gemini-3.1-pro-preview",
		Timeout:        5 * time.Second,
		StdoutFile:     out,
		StderrFile:     errf,
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	policyPath := readPolicyPath(t, out)
	if policyPath == "" {
		t.Fatal("stub did not record --policy path")
	}
	policyDir := filepath.Dir(policyPath)
	if _, err := os.Stat(policyDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("policy dir %q still exists after Execute (stat err: %v)", policyDir, err)
	}
}

// TestExecuteFreshPolicyDirPerCall verifies two consecutive Execute
// calls with bypassPermissions produce distinct policy dir paths — the
// dir is scoped to one invocation, not memoized.
func TestExecuteFreshPolicyDirPerCall(t *testing.T) {
	stub := writeStub(t)
	g := &Gemini{Binary: stub}

	tmp := t.TempDir()
	out1 := filepath.Join(tmp, "out1")
	out2 := filepath.Join(tmp, "out2")
	errf := filepath.Join(tmp, "err")

	for _, out := range []string{out1, out2} {
		_, err := g.Execute(context.Background(), executor.Request{
			Prompt:         "x",
			Model:          "gemini-3.1-pro-preview",
			Timeout:        5 * time.Second,
			StdoutFile:     out,
			StderrFile:     errf,
			PermissionMode: "bypassPermissions",
		})
		if err != nil {
			t.Fatalf("Execute (%s): %v", out, err)
		}
	}

	p1 := readPolicyPath(t, out1)
	p2 := readPolicyPath(t, out2)
	if p1 == "" || p2 == "" {
		t.Fatalf("missing --policy records: p1=%q p2=%q", p1, p2)
	}
	if filepath.Dir(p1) == filepath.Dir(p2) {
		t.Errorf("policy dir reused across calls: %q", filepath.Dir(p1))
	}
}

// TestExecuteDoesNotSetGeminiCliHome verifies the executor preserves
// the ambient $GEMINI_CLI_HOME (or its absence) so gemini-cli reads
// OAuth credentials from the user's real `~/.gemini/`. Setting
// GEMINI_CLI_HOME to a fresh dir would mask `~/.gemini/oauth_creds.json`
// and break authentication for OAuth users.
func TestExecuteDoesNotSetGeminiCliHome(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "envprobe.sh")
	body := `#!/bin/sh
echo "GEMINI_CLI_HOME=${GEMINI_CLI_HOME-<unset>}"
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	g := &Gemini{Binary: stub}

	out := filepath.Join(dir, "out")
	errf := filepath.Join(dir, "err")
	t.Setenv("GEMINI_CLI_HOME", "")
	_ = os.Unsetenv("GEMINI_CLI_HOME")

	_, err := g.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "gemini-3.1-pro-preview",
		Timeout:    5 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if !strings.Contains(string(data), "GEMINI_CLI_HOME=<unset>") {
		t.Errorf("subprocess saw GEMINI_CLI_HOME set; executor must not redirect gemini's home dir.\nstdout: %q", string(data))
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
	g := &Gemini{Binary: stub}

	_, err := g.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "gemini-3.1-pro-preview",
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
	g := &Gemini{Binary: stub}

	resp, err := g.Execute(context.Background(), executor.Request{
		Prompt:     "x",
		Model:      "gemini-3.1-pro-preview",
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

// TestExecuteWrapsRateLimitMarkers covers each gemini rate-limit
// pattern. The executor must wrap into *runner.LimitError with
// Tool="gemini-cli" and HelpCmd=geminiHelpCmd.
func TestExecuteWrapsRateLimitMarkers(t *testing.T) {
	for _, pattern := range geminiLimitPatterns {
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
			g := &Gemini{Binary: stub}

			_, err := g.Execute(context.Background(), executor.Request{
				Prompt:     "x",
				Model:      "gemini-3.1-pro-preview",
				Timeout:    5 * time.Second,
				StdoutFile: filepath.Join(dir, "out"),
				StderrFile: filepath.Join(dir, "err"),
			})
			var le *runner.LimitError
			if !errors.As(err, &le) {
				t.Fatalf("err = %v, want *runner.LimitError", err)
			}
			if le.Tool != "gemini-cli" {
				t.Errorf("LimitError.Tool = %q, want gemini-cli", le.Tool)
			}
			if le.HelpCmd != geminiHelpCmd {
				t.Errorf("LimitError.HelpCmd = %q, want %q", le.HelpCmd, geminiHelpCmd)
			}
			if le.Pattern != pattern {
				t.Errorf("LimitError.Pattern = %q, want %q", le.Pattern, pattern)
			}
		})
	}
}

func TestInitRegistersGemini(t *testing.T) {
	got, err := executor.Get("gemini-cli")
	if err != nil {
		t.Fatalf("Get(gemini-cli) after import: %v", err)
	}
	if got.Name() != "gemini-cli" {
		t.Errorf("registered Name() = %q, want gemini-cli", got.Name())
	}
}
