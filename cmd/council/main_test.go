package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/session"
)

// stubExec is a copy-lite of the orchestrator's stubExec — we only need
// a tiny version for the CLI-level happy/quorum/judge path tests. The
// registered executor is named "claude-code" so the test profiles can
// reuse the shipped default-profile YAML verbatim.
type stubExec struct {
	name     string
	onExpert func(stdoutFile, stderrFile string) (int, error)
	onJudge  func(stdoutFile, stderrFile string) (int, error)
	calls    int64
}

func (s *stubExec) Name() string { return s.name }
func (s *stubExec) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	atomic.AddInt64(&s.calls, 1)
	if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
		code, err := s.onJudge(req.StdoutFile, req.StderrFile)
		return executor.Response{ExitCode: code, Duration: time.Millisecond}, err
	}
	code, err := s.onExpert(req.StdoutFile, req.StderrFile)
	return executor.Response{ExitCode: code, Duration: time.Millisecond}, err
}

// writeStdout is a convenience for stub callbacks that return success.
func writeStdout(body string) func(stdoutFile, stderrFile string) (int, error) {
	return func(stdoutFile, _ string) (int, error) {
		return 0, os.WriteFile(stdoutFile, []byte(body), 0o644)
	}
}

// registerStub swaps in the stub executor under name "claude-code" for
// the duration of the test so the shipped default profile YAML (which
// names executor: claude-code) resolves to our test double.
func registerStub(t *testing.T, s *stubExec) {
	t.Helper()
	executor.ResetForTest()
	executor.Register(s)
	t.Cleanup(func() { executor.ResetForTest() })
}

// withCouncilDir creates a .council/default.yaml + prompts/{judge,
// independent,critic}.md in dir, returning dir. The YAML is the v1
// MVP default profile from docs/design/v1.md §5 with a short timeout so
// test failures abort quickly instead of hanging the suite.
func withCouncilDir(t *testing.T, dir string) string {
	t.Helper()
	councilDir := filepath.Join(dir, ".council")
	promptsDir := filepath.Join(councilDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range map[string]string{
		"judge.md":       "You are the judge.\n",
		"independent.md": "You are independent.\n",
		"critic.md":      "You are critic.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write prompt %s: %v", name, err)
		}
	}
	yaml := `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/judge.md
  timeout: 5s
experts:
  - name: independent
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 5s
  - name: critic
    executor: claude-code
    model: sonnet
    prompt_file: prompts/critic.md
    timeout: 5s
quorum: 1
max_retries: 0
`
	if err := os.WriteFile(filepath.Join(councilDir, "default.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return dir
}

// freezeTimestamp pins nowStamp so verbose output is deterministic; tests
// that care about the prefix can match exact text.
func freezeTimestamp(t *testing.T, stamp string) {
	t.Helper()
	orig := nowStamp
	nowStamp = func() string { return stamp }
	t.Cleanup(func() { nowStamp = orig })
}

// TestRun_Version verifies --version prints the expected line and exits 0.
func TestRun_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--version"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if !strings.HasPrefix(stdout.String(), "council ") {
		t.Errorf("stdout = %q, want prefix 'council '", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr non-empty: %q", stderr.String())
	}
}

// TestRun_Help exits 0 via flag.ErrHelp and writes the usage to stderr.
func TestRun_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	// Flag parser writes usage on help; we also print our own help from
	// the Usage hook. Either way stderr should describe the flags.
	if !strings.Contains(stderr.String(), "--profile") {
		t.Errorf("stderr missing --profile hint: %q", stderr.String())
	}
}

// TestRun_UnknownFlag exits 1 because flag parsing failed.
func TestRun_UnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--nope"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfigError {
		t.Errorf("exit = %d, want %d", code, exitConfigError)
	}
}

// TestRun_NonDefaultProfileRejected exits 1 with a clear message when -p
// names anything other than "default". v1 ships a single profile per
// location; the flag exists to keep the surface stable for v2.
func TestRun_NonDefaultProfileRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-p", "code-review", "q"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfigError {
		t.Errorf("exit = %d, want %d", code, exitConfigError)
	}
	if !strings.Contains(stderr.String(), "code-review") {
		t.Errorf("stderr missing bad profile name: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "not supported in v1") {
		t.Errorf("stderr missing v1-limitation hint: %q", stderr.String())
	}
}

// TestRun_MissingQuestion exits 1 when no positional is given.
func TestRun_MissingQuestion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfigError {
		t.Errorf("exit = %d, want %d", code, exitConfigError)
	}
	if !strings.Contains(stderr.String(), "expected exactly one question") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRun_StdinDash reads the question from stdin when positional == "-".
func TestRun_StdinDash(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	stub := &stubExec{
		name:     "claude-code",
		onExpert: writeStdout("EXPERT_OUT"),
		onJudge:  writeStdout("FINAL from stdin run"),
	}
	registerStub(t, stub)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-"}, strings.NewReader("what is life?"), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "FINAL from stdin run") {
		t.Errorf("stdout = %q", stdout.String())
	}
	// Confirm the question reached disk (question.md) verbatim.
	sessions, _ := filepath.Glob(".council/sessions/*/question.md")
	if len(sessions) != 1 {
		t.Fatalf("expected 1 question.md, got %d (%v)", len(sessions), sessions)
	}
	b, err := os.ReadFile(sessions[0])
	if err != nil {
		t.Fatalf("read question.md: %v", err)
	}
	if string(b) != "what is life?" {
		t.Errorf("question.md = %q, want %q", b, "what is life?")
	}
}

// TestRun_HappyPath is the end-to-end success case per plan Task 7.
// Asserts exit 0, stdout == synthesis body + newline, verdict.json
// present on disk with status=ok.
func TestRun_HappyPath(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	stub := &stubExec{
		name:     "claude-code",
		onExpert: writeStdout("EXPERT_OUT"),
		onJudge:  writeStdout("FINAL_ANSWER"),
	}
	registerStub(t, stub)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-p", "default", "what is 2+2?"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if stdout.String() != "FINAL_ANSWER\n" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "FINAL_ANSWER\n")
	}
	verdicts, _ := filepath.Glob(".council/sessions/*/verdict.json")
	if len(verdicts) != 1 {
		t.Fatalf("expected 1 verdict.json, got %d", len(verdicts))
	}
	b, _ := os.ReadFile(verdicts[0])
	if !strings.Contains(string(b), `"status": "ok"`) {
		t.Errorf("verdict.json missing status=ok: %s", b)
	}
}

// TestRun_Verbose verifies verbose mode emits the documented §11 preamble
// + per-role timing lines to stderr, and still writes the synthesis to
// stdout.
func TestRun_Verbose(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	freezeTimestamp(t, "17:02:14")
	stub := &stubExec{
		name:     "claude-code",
		onExpert: writeStdout("EXPERT_OUT"),
		onJudge:  writeStdout("FINAL"),
	}
	registerStub(t, stub)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-v", "q"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d (stderr=%s)", code, stderr.String())
	}
	for _, want := range []string{
		"[17:02:14] council",
		"profile: default",
		"spawning expert: independent",
		"spawning expert: critic",
		"spawning judge",
		"judge: done",
		"session folder:",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q; got %s", want, stderr.String())
		}
	}
}

// TestRun_QuorumFailed: every expert fails, quorum unmet → exit 2 and
// the stderr hint points at verdict.json. Verdict.json is on disk with
// status=quorum_failed.
func TestRun_QuorumFailed(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	stub := &stubExec{
		name: "claude-code",
		onExpert: func(_, stderrFile string) (int, error) {
			_ = os.WriteFile(stderrFile, []byte("expert down\n"), 0o644)
			return 1, errors.New("expert fail")
		},
		onJudge: func(_, _ string) (int, error) {
			return 0, errors.New("judge should not run")
		},
	}
	registerStub(t, stub)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"q"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitQuorum {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitQuorum, stderr.String())
	}
	if !strings.Contains(stderr.String(), "quorum not met") {
		t.Errorf("stderr missing quorum hint: %q", stderr.String())
	}
	verdicts, _ := filepath.Glob(".council/sessions/*/verdict.json")
	if len(verdicts) != 1 {
		t.Fatalf("verdict.json count = %d", len(verdicts))
	}
	b, _ := os.ReadFile(verdicts[0])
	if !strings.Contains(string(b), `"status": "quorum_failed"`) {
		t.Errorf("verdict.json missing quorum_failed: %s", b)
	}
}

// TestRun_JudgeFailed: experts succeed, judge always fails → exit 3.
func TestRun_JudgeFailed(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	stub := &stubExec{
		name:     "claude-code",
		onExpert: writeStdout("EXPERT_OUT"),
		onJudge: func(_, stderrFile string) (int, error) {
			_ = os.WriteFile(stderrFile, []byte("judge down\n"), 0o644)
			return 1, errors.New("judge fail")
		},
	}
	registerStub(t, stub)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"q"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitJudge {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitJudge, stderr.String())
	}
	if !strings.Contains(stderr.String(), "judge failed") {
		t.Errorf("stderr missing judge hint: %q", stderr.String())
	}
}

// TestRun_InterruptedViaContext simulates SIGINT by cancelling the root
// context before the orchestrator returns. The stub blocks on ctx.Done
// for the in-flight expert, so cancellation forces the orchestrator to
// write an interrupted verdict. This is the in-process equivalent of the
// "kill mid-run" assertion — the key guarantees are (1) exit code 130,
// (2) verdict.json on disk with status=interrupted BEFORE run returns.
func TestRun_InterruptedViaContext(t *testing.T) {
	t.Chdir(withCouncilDir(t, t.TempDir()))
	started := make(chan struct{}, 2)
	executor.ResetForTest()
	executor.Register(&interruptibleStub{name: "claude-code", started: started})
	t.Cleanup(func() { executor.ResetForTest() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		<-started
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"q"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitInterrupted {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitInterrupted, stderr.String())
	}
	verdicts, _ := filepath.Glob(".council/sessions/*/verdict.json")
	if len(verdicts) != 1 {
		t.Fatalf("verdict.json count = %d", len(verdicts))
	}
	b, _ := os.ReadFile(verdicts[0])
	if !strings.Contains(string(b), `"status": "interrupted"`) {
		t.Errorf("verdict.json missing interrupted status: %s", b)
	}
}

// interruptibleStub blocks inside Execute until the context cancels, so
// a test can drive the interrupted path deterministically.
type interruptibleStub struct {
	name    string
	started chan struct{}
}

func (s *interruptibleStub) Name() string { return s.name }
func (s *interruptibleStub) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return executor.Response{ExitCode: 0}, ctx.Err()
}

// TestReadQuestion covers the "-" vs literal-arg branching without
// touching the orchestrator.
func TestReadQuestion(t *testing.T) {
	t.Run("literal", func(t *testing.T) {
		q, err := readQuestion("hello", strings.NewReader("ignored"))
		if err != nil || q != "hello" {
			t.Errorf("got %q, %v; want hello, nil", q, err)
		}
	})
	t.Run("stdin-dash", func(t *testing.T) {
		q, err := readQuestion("-", strings.NewReader("from-stdin\n"))
		if err != nil || q != "from-stdin\n" {
			t.Errorf("got %q, %v; want from-stdin, nil", q, err)
		}
	})
}

// TestFlagParsing is a table-driven smoke of the shorthand + long forms
// exposed in docs/design/v1.md §4.
func TestFlagParsing(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want int
	}{
		{"--version", []string{"--version"}, exitOK},
		{"--help", []string{"--help"}, exitOK},
		{"-v no question", []string{"-v"}, exitConfigError},
		{"-p with value no question", []string{"-p", "default"}, exitConfigError},
		{"--profile=default no question", []string{"--profile=default"}, exitConfigError},
		{"unknown flag", []string{"--bogus"}, exitConfigError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := run(context.Background(), tc.argv, strings.NewReader(""), &stdout, &stderr)
			if got != tc.want {
				t.Errorf("exit = %d, want %d (stderr=%s)", got, tc.want, stderr.String())
			}
		})
	}
}

// TestResolveVersion covers the -ldflags override path; the debug.BuildInfo
// branch only fires under `go install` and is exercised in Post-Completion
// manual verification.
func TestResolveVersion(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	version = "v1.2.3-test"
	if got := resolveVersion(); got != "v1.2.3-test" {
		t.Errorf("resolveVersion = %q, want v1.2.3-test", got)
	}
}

// TestDisplaySource covers the three cases the verbose preamble renders:
// the literal "embedded" sentinel, a config file inside the session's cwd
// (relative), and a config file outside the cwd (absolute).
func TestDisplaySource(t *testing.T) {
	cwd := t.TempDir()
	sessPath := filepath.Join(cwd, ".council", "sessions", "sess-id")
	sess := &session.Session{Path: sessPath}

	if got := displaySource("embedded", sess); got != "embedded" {
		t.Errorf("embedded source: got %q, want embedded", got)
	}
	insideAbs := filepath.Join(cwd, ".council", "default.yaml")
	if got := displaySource(insideAbs, sess); got != filepath.Join(".council", "default.yaml") {
		t.Errorf("inside source: got %q, want .council/default.yaml", got)
	}
	outsideAbs := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if got := displaySource(outsideAbs, sess); got != outsideAbs {
		t.Errorf("outside source: got %q, want %q (absolute passthrough)", got, outsideAbs)
	}
}
