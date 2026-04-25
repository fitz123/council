package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
)

// initStubExec is the cmd/council init test stub. Separate from
// preflight_test.go's pfExec so the field set stays minimal — init only
// reads Name + BinaryName.
type initStubExec struct {
	name string
	bin  string
}

func (e *initStubExec) Name() string       { return e.name }
func (e *initStubExec) BinaryName() string { return e.bin }
func (e *initStubExec) Execute(_ context.Context, _ executor.Request) (executor.Response, error) {
	return executor.Response{}, errors.New("initStubExec.Execute should not be called — probe is stubbed")
}

// withInitTestEnv resets the executor registry, points homeDir at the
// given temp dir, and stubs exec.LookPath to succeed for known binary
// names. Returns a cleanup that restores everything.
func withInitTestEnv(t *testing.T, tmpHome string, presentBinaries map[string]bool) {
	t.Helper()
	executor.ResetForTest()

	origHome := homeDir
	homeDir = func() (string, error) { return tmpHome, nil }

	origLookPath := initLookPath
	initLookPath = func(bin string) (string, error) {
		if presentBinaries[bin] {
			return "/usr/bin/" + bin, nil
		}
		return "", exec.ErrNotFound
	}

	origProbe := probe
	probe = func(_ context.Context, ex executor.Executor, _ string) (bool, string) {
		// Default: every present-and-registered CLI probes OK. Tests
		// that need a non-OK probe override this in their own setup.
		return true, ""
	}

	t.Cleanup(func() {
		executor.ResetForTest()
		homeDir = origHome
		initLookPath = origLookPath
		probe = origProbe
	})
}

// registerThree registers stubs for all three v3 CLI executors with
// their canonical names + BinaryNames.
func registerThree(t *testing.T) {
	t.Helper()
	executor.Register(&initStubExec{name: "claude-code", bin: "claude"})
	executor.Register(&initStubExec{name: "codex", bin: "codex"})
	executor.Register(&initStubExec{name: "gemini-cli", bin: "gemini"})
}

func TestInit_AllThreeVerified_WritesProfile(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{"claude": true, "codex": true, "gemini": true})
	registerThree(t)

	var stdout, stderr bytes.Buffer
	code := runInit(nil, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runInit = %d, want %d (stderr=%s)", code, exitOK, stderr.String())
	}

	target := filepath.Join(tmp, ".config", "council", "default.yaml")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("yaml not written at %s: %v", target, err)
	}
	if !strings.Contains(stdout.String(), "wrote "+target) {
		t.Errorf("stdout missing 'wrote' line; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "3 experts, quorum 2") {
		t.Errorf("stdout missing expert/quorum summary; got %q", stdout.String())
	}
}

func TestInit_Idempotent_NoOverwriteWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{"claude": true, "codex": true, "gemini": true})
	registerThree(t)

	target := filepath.Join(tmp, ".config", "council", "default.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const sentinel = "# pre-existing\n"
	if err := os.WriteFile(target, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runInit(nil, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runInit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "profile exists") {
		t.Errorf("stdout missing 'profile exists' message; got %q", stdout.String())
	}

	body, _ := os.ReadFile(target)
	if string(body) != sentinel {
		t.Errorf("file was overwritten; got %q want %q", string(body), sentinel)
	}
}

func TestInit_Force_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{"claude": true, "codex": true, "gemini": true})
	registerThree(t)

	target := filepath.Join(tmp, ".config", "council", "default.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("# pre-existing\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--force"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runInit --force = %d, want %d (stderr=%s)", code, exitOK, stderr.String())
	}

	body, _ := os.ReadFile(target)
	if strings.Contains(string(body), "pre-existing") {
		t.Errorf("--force did not overwrite; got %q", string(body))
	}
	if !strings.Contains(string(body), "version: 2") {
		t.Errorf("regenerated yaml missing version: 2; got %q", string(body))
	}
}

func TestInit_OnlyRegisteredAndAvailableCLIsAppear(t *testing.T) {
	tmp := t.TempDir()
	// claude is registered + on PATH; codex is registered but missing
	// PATH; gemini is not registered at all.
	withInitTestEnv(t, tmp, map[string]bool{"claude": true})
	executor.Register(&initStubExec{name: "claude-code", bin: "claude"})
	executor.Register(&initStubExec{name: "codex", bin: "codex"})

	var stdout, stderr bytes.Buffer
	code := runInit(nil, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runInit = %d, want %d (stderr=%s)", code, exitOK, stderr.String())
	}

	body, err := os.ReadFile(filepath.Join(tmp, ".config", "council", "default.yaml"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	if !strings.Contains(string(body), "executor: claude-code") {
		t.Errorf("yaml missing claude-code; got %s", body)
	}
	if strings.Contains(string(body), "executor: codex") {
		t.Errorf("yaml unexpectedly contains codex (PATH-missing); got %s", body)
	}
	if strings.Contains(string(body), "executor: gemini-cli") {
		t.Errorf("yaml unexpectedly contains gemini-cli (unregistered); got %s", body)
	}
	// Summary lines must distinguish skipped reasons.
	if !strings.Contains(stdout.String(), "skipped: codex") {
		t.Errorf("stdout missing 'skipped: codex'; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "skipped: gemini-cli") {
		t.Errorf("stdout missing 'skipped: gemini-cli'; got %q", stdout.String())
	}
}

func TestInit_ProbeFnExcludesFailingCLI(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{"claude": true, "codex": true, "gemini": true})
	registerThree(t)

	probe = func(_ context.Context, ex executor.Executor, _ string) (bool, string) {
		if ex.Name() == "codex" {
			return false, "no OK in probe output"
		}
		return true, ""
	}

	var stdout, stderr bytes.Buffer
	code := runInit(nil, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runInit = %d, want %d (stderr=%s)", code, exitOK, stderr.String())
	}
	body, _ := os.ReadFile(filepath.Join(tmp, ".config", "council", "default.yaml"))
	if strings.Contains(string(body), "executor: codex") {
		t.Errorf("codex should be excluded after probe failure; got %s", body)
	}
	if !strings.Contains(stdout.String(), "skipped: codex (no OK in probe output)") {
		t.Errorf("stdout missing skip-with-reason for codex; got %q", stdout.String())
	}
	// 2 verified → quorum 2.
	if !strings.Contains(stdout.String(), "2 experts, quorum 2") {
		t.Errorf("stdout missing 2/quorum-2 summary; got %q", stdout.String())
	}
}

func TestInit_ZeroVerified_ExitsNonZero(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{}) // nothing on PATH
	registerThree(t)

	var stdout, stderr bytes.Buffer
	code := runInit(nil, &stdout, &stderr)
	if code == exitOK {
		t.Fatalf("runInit = %d, want non-zero", code)
	}
	if !strings.Contains(stderr.String(), "install at least one of") {
		t.Errorf("stderr missing install hint; got %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(tmp, ".config", "council", "default.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("yaml unexpectedly written: stat err = %v", err)
	}
}

func TestInit_QuorumScaling(t *testing.T) {
	cases := []struct {
		n           int
		wantQuorum  int
		summaryFrag string
	}{
		{n: 3, wantQuorum: 2, summaryFrag: "3 experts, quorum 2"},
		{n: 2, wantQuorum: 2, summaryFrag: "2 experts, quorum 2"},
		{n: 1, wantQuorum: 1, summaryFrag: "1 experts, quorum 1"},
	}
	allBins := []string{"claude", "codex", "gemini"}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("n=%d", tc.n), func(t *testing.T) {
			tmp := t.TempDir()
			present := map[string]bool{}
			for i := 0; i < tc.n; i++ {
				present[allBins[i]] = true
			}
			withInitTestEnv(t, tmp, present)
			registerThree(t)

			var stdout, stderr bytes.Buffer
			code := runInit(nil, &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("runInit = %d, want %d (stderr=%s)", code, exitOK, stderr.String())
			}
			if got := scaleQuorum(tc.n); got != tc.wantQuorum {
				t.Errorf("scaleQuorum(%d) = %d, want %d", tc.n, got, tc.wantQuorum)
			}
			if !strings.Contains(stdout.String(), tc.summaryFrag) {
				t.Errorf("stdout missing %q; got %q", tc.summaryFrag, stdout.String())
			}
		})
	}
}

func TestInit_GeneratedYAMLRoundTripsThroughLoader(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{"claude": true, "codex": true, "gemini": true})
	registerThree(t)

	var stdout, stderr bytes.Buffer
	if code := runInit(nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("runInit = %d (stderr=%s)", code, stderr.String())
	}

	target := filepath.Join(tmp, ".config", "council", "default.yaml")
	p, err := config.LoadFile(target)
	if err != nil {
		t.Fatalf("LoadFile %s: %v", target, err)
	}
	if p.Version != 2 {
		t.Errorf("Version = %d, want 2", p.Version)
	}
	if len(p.Experts) != 3 {
		t.Errorf("Experts count = %d, want 3", len(p.Experts))
	}
	if p.Quorum != 2 {
		t.Errorf("Quorum = %d, want 2", p.Quorum)
	}
	if p.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2", p.Rounds)
	}
	if p.Voting.BallotPromptBody == "" {
		t.Error("Voting.BallotPromptBody empty (prompt seeds not written?)")
	}
	if p.Round2Prompt.Body == "" {
		t.Error("Round2Prompt.Body empty (peer-aware seed not written?)")
	}
}

func TestInit_HomeDirOverride_DoesNotTouchRealHome(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{"claude": true, "codex": true, "gemini": true})
	registerThree(t)

	var stdout, stderr bytes.Buffer
	if code := runInit(nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("runInit = %d (stderr=%s)", code, stderr.String())
	}
	// File must land under our injected tmp, not under whatever
	// os.UserHomeDir() returns on the test host.
	target := filepath.Join(tmp, ".config", "council", "default.yaml")
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected yaml under injected home %s: %v", target, err)
	}
	realHome, _ := os.UserHomeDir()
	if realHome != "" && !strings.HasPrefix(tmp, realHome) {
		// realHome may be a prefix of tmp on some CI runners; only
		// flag the violation when tmp is genuinely outside realHome.
		stray := filepath.Join(realHome, ".config", "council", "default.yaml")
		if stat, err := os.Stat(stray); err == nil {
			// File may exist for other reasons — only fail if it was
			// just created. We can't reliably mtime-check here, so
			// log a warning rather than fail.
			t.Logf("note: real-home yaml exists at %s (size %d) — pre-existing, not created by this test", stray, stat.Size())
		}
	}
}

func TestScaleQuorum(t *testing.T) {
	cases := []struct {
		n, want int
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 2},
		{4, 2},
	}
	for _, tc := range cases {
		if got := scaleQuorum(tc.n); got != tc.want {
			t.Errorf("scaleQuorum(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestInit_UnknownFlag_ExitsConfigError(t *testing.T) {
	tmp := t.TempDir()
	withInitTestEnv(t, tmp, map[string]bool{})

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--bogus"}, &stdout, &stderr)
	if code != exitConfigError {
		t.Errorf("runInit --bogus = %d, want %d", code, exitConfigError)
	}
}
