//go:build smoke

// Package smoke wires up the v2 debate-engine fitness functions
// (F1–F9, F12) from docs/plans/v2-debate-engine.md §15 as Go-level tests
// that shell out to the built binaries. It is gated by `-tags smoke` so
// a normal `go test ./...` does not pay the build cost.
//
// Two binaries are referenced via env vars (set by test/smoke/run.sh):
//
//   - COUNCIL_TEST_BINARY  — built with `-tags testbinary`; routes
//     "claude-code" to the in-process mock executor whose behavior is
//     selected by COUNCIL_MOCK_EXECUTOR. Used by F2–F9, F12.
//   - COUNCIL_RELEASE_BINARY — built without the testbinary tag; calls
//     the real `claude` CLI. Used only by F1, which is itself skipped
//     unless COUNCIL_LIVE_CLAUDE=1 is set.
//
// Each test uses a fresh t.TempDir() as cwd so its `.council/sessions/`
// folder is isolated from other tests' state.
package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// envTestBinary names the testbinary-tagged build of cmd/council.
const envTestBinary = "COUNCIL_TEST_BINARY"

// envReleaseBinary names the production build of cmd/council. Only F1
// uses it.
const envReleaseBinary = "COUNCIL_RELEASE_BINARY"

// envLive is the gate for tests that hit the real claude CLI.
const envLive = "COUNCIL_LIVE_CLAUDE"

// envMock selects which mock behavior the testbinary executor runs.
const envMock = "COUNCIL_MOCK_EXECUTOR"

// testBinary returns the path to the testbinary build of council, or
// fails the test if the env var is missing. Callers should run via
// test/smoke/run.sh which guarantees both vars are set.
func testBinary(t *testing.T) string {
	t.Helper()
	p := os.Getenv(envTestBinary)
	if p == "" {
		t.Fatalf("%s is not set; run test/smoke/run.sh instead of `go test` directly", envTestBinary)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s=%q: %v", envTestBinary, p, err)
	}
	return p
}

// releaseBinary returns the path to the release build, skipping the
// test if the live-claude gate is unset (since the only test using
// this binary is the live-claude one).
func releaseBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv(envLive) != "1" {
		t.Skipf("%s != 1, skipping live-claude test", envLive)
	}
	p := os.Getenv(envReleaseBinary)
	if p == "" {
		t.Fatalf("%s is not set", envReleaseBinary)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s=%q: %v", envReleaseBinary, p, err)
	}
	return p
}

// runResult captures the outcome of one council invocation. Stdout and
// stderr are returned as strings because the smoke assertions only do
// substring/regex checks; the body is small (council's stdout is a
// single answer).
type runResult struct {
	stdout   string
	stderr   string
	exitCode int
	cwd      string
	duration time.Duration
}

// runOpts groups all knobs runCouncil cares about. Defaults: no extra
// env beyond inherited, no stdin, 60 s deadline, fresh t.TempDir cwd.
type runOpts struct {
	binary   string            // required
	args     []string          // positional+flags after the binary path
	stdin    string            // piped to council's stdin (empty = no stdin)
	env      map[string]string // appended to inherited env
	cwd      string            // empty → t.TempDir()
	deadline time.Duration     // 0 → 60s
}

// runCouncil execs the binary with opts. Returns the captured outcome.
// The deadline kills the child via context cancellation if it overruns
// — this protects the suite from a hung child blocking the whole run.
func runCouncil(t *testing.T, opts runOpts) runResult {
	t.Helper()
	if opts.cwd == "" {
		opts.cwd = t.TempDir()
	}
	res, err := runCouncilResult(opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func runCouncilResult(opts runOpts) (runResult, error) {
	if opts.binary == "" {
		return runResult{}, errors.New("runCouncil: binary required")
	}
	if opts.deadline == 0 {
		opts.deadline = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.deadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, opts.binary, opts.args...)
	cmd.Dir = opts.cwd
	if opts.stdin != "" {
		cmd.Stdin = strings.NewReader(opts.stdin)
	}
	cmd.Env = append(os.Environ(), envSlice(opts.env)...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	res := runResult{
		stdout:   outBuf.String(),
		stderr:   errBuf.String(),
		cwd:      opts.cwd,
		duration: dur,
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.exitCode = ee.ExitCode()
		} else if ctx.Err() != nil {
			return res, fmt.Errorf("council deadline %s exceeded; stderr:\n%s", opts.deadline, errBuf.String())
		} else {
			return res, fmt.Errorf("council exec failed: %v\nstderr:\n%s", err, errBuf.String())
		}
	}
	return res, nil
}

// envSlice converts a map to KEY=VAL form, suitable for appending to
// os.Environ().
func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// onlySession returns the single session folder under cwd/.council/
// sessions/. Smoke tests run in fresh temp dirs so there should be
// exactly one. Returns the path to the session folder.
func onlySession(t *testing.T, cwd string) string {
	t.Helper()
	root := filepath.Join(cwd, ".council", "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one session under %s, got %d", root, len(entries))
	}
	return filepath.Join(root, entries[0].Name())
}

// readVerdict reads and decodes verdict.json from the given session
// folder. Returns the parsed JSON as a generic map so the smoke layer
// does not import the session package (decoupling: the smoke contract
// is the JSON shape, not the Go struct).
func readVerdict(t *testing.T, sessionPath string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionPath, "verdict.json"))
	if err != nil {
		t.Fatalf("read verdict.json at %s: %v", sessionPath, err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode verdict.json: %v\n%s", err, b)
	}
	return v
}

// writeProfile lays down a minimal .council/default.yaml + the named
// prompt files inside cwd. Used by F7a/F7b which need to inject a
// broken config that overrides the embedded defaults. yamlBody is
// written verbatim. prompts maps relative filename → body.
func writeProfile(t *testing.T, cwd, yamlBody string, prompts map[string]string) {
	t.Helper()
	dir := filepath.Join(cwd, ".council")
	pdir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range prompts {
		if err := os.WriteFile(filepath.Join(pdir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write prompt %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write default.yaml: %v", err)
	}
}

// validProfileYAML is a minimal v2 default profile pointing at relative
// prompt files; used as a base for F7a/F7b which mutate one piece of it
// to verify a specific rejection. Matches the shape of defaults/default.yaml.
const validProfileYAML = `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_3
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 1
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
  timeout: 180s
`

func defaultPrompts() map[string]string {
	return map[string]string{
		"independent.md": "you are independent.\n",
		"ballot.md":      "you are a voter. output VOTE: <label>.\n",
	}
}

// ----- Fitness functions (F1–F9, F12) -----

// TestF1_LiveHappyPath: real `claude` CLI exits 0 within 120 s on the
// default v2 profile for "what is 2+2?". Skipped unless
// COUNCIL_LIVE_CLAUDE=1 (CI does not have the CLI; local smoke runs
// do). v2 makes two sub-calls per expert (R1 + R2) plus one ballot, so
// the deadline is bumped vs v1's 60 s.
func TestF1_LiveHappyPath(t *testing.T) {
	bin := releaseBinary(t)
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"what is 2+2?"},
		deadline: 120 * time.Second,
	})
	if res.exitCode != 0 {
		t.Fatalf("F1: exit %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if strings.TrimSpace(res.stdout) == "" {
		t.Fatalf("F1: empty stdout")
	}
}

// TestF2_LiveVerdictShape composes with F1: after a real run, the
// verdict.json must include the v2 top-level keys. Same gate as F1
// because it depends on the live CLI.
func TestF2_LiveVerdictShape(t *testing.T) {
	bin := releaseBinary(t)
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"what is 2+2?"},
		deadline: 120 * time.Second,
	})
	if res.exitCode != 0 {
		t.Fatalf("F2: exit %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	v := readVerdict(t, onlySession(t, res.cwd))
	for _, key := range []string{"version", "session_id", "answer", "status", "rounds", "voting", "anonymization"} {
		if _, ok := v[key]; !ok {
			t.Errorf("F2: missing top-level key %q in verdict.json", key)
		}
	}
	if got, _ := v["version"].(float64); got != 2 {
		t.Errorf("F2: version = %v, want 2", v["version"])
	}
	if v["status"] != "ok" {
		t.Errorf("F2: status = %v, want ok", v["status"])
	}
	rounds, _ := v["rounds"].([]any)
	if len(rounds) != 2 {
		t.Fatalf("F2: rounds = %d, want 2", len(rounds))
	}
	for i, r := range rounds {
		rm, _ := r.(map[string]any)
		if _, ok := rm["experts"]; !ok {
			t.Errorf("F2: rounds[%d].experts missing", i)
		}
	}
}

// TestF3_ConcurrentDistinctSessions: three council invocations in
// parallel produce three distinct session IDs. Uses the trivial mock so
// the test does not depend on the live CLI; the petname suffix is what
// guarantees uniqueness when the timestamps collide at second resolution.
func TestF3_ConcurrentDistinctSessions(t *testing.T) {
	bin := testBinary(t)
	const n = 3
	cwds := make([]string, n)
	for i := range cwds {
		cwds[i] = t.TempDir()
	}
	var wg sync.WaitGroup
	results := make([]runResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("panic: %v", r)
				}
			}()
			res, err := runCouncilResult(runOpts{
				binary:   bin,
				args:     []string{"hello"},
				env:      map[string]string{envMock: "trivial"},
				cwd:      cwds[i],
				deadline: 30 * time.Second,
			})
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	ids := map[string]bool{}
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("F3 invocation %d: %v", i, errs[i])
		}
		if res.exitCode != 0 {
			t.Fatalf("F3 invocation %d: exit %d\nstderr:\n%s", i, res.exitCode, res.stderr)
		}
		sess := onlySession(t, cwds[i])
		id := filepath.Base(sess)
		if ids[id] {
			t.Fatalf("F3: duplicate session id %q across invocations", id)
		}
		ids[id] = true
	}
	if len(ids) != n {
		t.Fatalf("F3: got %d distinct session ids, want %d", len(ids), n)
	}
}

// TestF4_RetryRecorded: the fail_once_then_ok mock makes every expert
// fail on its first attempt and succeed on the retry. With max_retries=1
// in the default profile, verdict.json must record at least one expert
// with retries=1 in round 0 (R1). v2 has two rounds; F4 only checks R1.
func TestF4_RetryRecorded(t *testing.T) {
	bin := testBinary(t)
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"hello"},
		env:      map[string]string{envMock: "fail_once_then_ok"},
		deadline: 30 * time.Second,
	})
	if res.exitCode != 0 {
		t.Fatalf("F4: exit %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	v := readVerdict(t, onlySession(t, res.cwd))
	if v["status"] != "ok" {
		t.Fatalf("F4: status = %v, want ok\nverdict:\n%v", v["status"], v)
	}
	rounds, _ := v["rounds"].([]any)
	if len(rounds) == 0 {
		t.Fatalf("F4: no rounds in verdict")
	}
	r0, _ := rounds[0].(map[string]any)
	experts, _ := r0["experts"].([]any)
	gotRetry := false
	for _, e := range experts {
		em, _ := e.(map[string]any)
		if r, ok := em["retries"].(float64); ok && int(r) == 1 {
			gotRetry = true
			break
		}
	}
	if !gotRetry {
		t.Fatalf("F4: no expert with retries=1 in verdict rounds[0]\nexperts:\n%v", experts)
	}
}

// TestF5_SIGINTInterrupted: launch the slow mock; after 3s send SIGINT;
// the process must exit 130 with verdict.json status=interrupted on
// disk. The root-level .done marker MUST be absent so `council resume`
// can pick the session up later (D14).
func TestF5_SIGINTInterrupted(t *testing.T) {
	bin := testBinary(t)
	cwd := t.TempDir()

	cmd := exec.Command(bin, "slow")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), envMock+"=slow")
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("F5: start: %v", err)
	}

	// Send SIGINT after 3 seconds, then wait for exit. A 30 s overall
	// deadline guards against the child ignoring the signal entirely.
	timer := time.AfterFunc(3*time.Second, func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
	})
	defer timer.Stop()
	deadline := time.AfterFunc(30*time.Second, func() {
		_ = cmd.Process.Kill()
	})
	defer deadline.Stop()

	err := cmd.Wait()
	exitCode := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("F5: wait: %v\nstderr:\n%s", err, errBuf.String())
	}
	if exitCode != 130 {
		t.Fatalf("F5: exit %d, want 130\nstderr:\n%s", exitCode, errBuf.String())
	}
	sess := onlySession(t, cwd)
	v := readVerdict(t, sess)
	if v["status"] != "interrupted" {
		t.Fatalf("F5: status = %v, want interrupted\nverdict:\n%v", v["status"], v)
	}
	// Finality signal must be absent so `council resume` picks this up.
	if _, err := os.Stat(filepath.Join(sess, ".done")); err == nil {
		t.Fatalf("F5: root .done marker should be absent on interrupt")
	}
}

// TestF6_LargePromptThroughStdin: pipe 200 KB of "y\n" into the
// stdin-dash form of the CLI, and verify (a) question.md contains the
// full body, AND (b) at least one expert's output.md reports a
// stdin-byte count >= len(question). Together these prove the bytes
// flowed all the way from the council process's stdin into the
// subprocess's stdin (not just onto disk).
func TestF6_LargePromptThroughStdin(t *testing.T) {
	bin := testBinary(t)
	question := strings.Repeat("y\n", 100_000) // 200 000 bytes exactly
	if len(question) != 200_000 {
		t.Fatalf("F6: question length %d, want 200000", len(question))
	}
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"-"},
		stdin:    question,
		env:      map[string]string{envMock: "echo-stdin-length"},
		deadline: 30 * time.Second,
	})
	if res.exitCode != 0 {
		t.Fatalf("F6: exit %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	sess := onlySession(t, res.cwd)
	qBody, err := os.ReadFile(filepath.Join(sess, "question.md"))
	if err != nil {
		t.Fatalf("F6: read question.md: %v", err)
	}
	if len(qBody) != len(question) {
		t.Fatalf("F6: question.md size %d, want %d", len(qBody), len(question))
	}
	// Walk every R1 expert's output.md and confirm the echoed byte count
	// is at least the question's size — the BUILT prompt the executor
	// receives wraps the question with role body + delimiters, so the
	// reported number is always ≥ len(question).
	expertsRoot := filepath.Join(sess, "rounds", "1", "experts")
	expertDirs, err := os.ReadDir(expertsRoot)
	if err != nil {
		t.Fatalf("F6: read experts dir: %v", err)
	}
	if len(expertDirs) == 0 {
		t.Fatalf("F6: no expert dirs found")
	}
	rxN := regexp.MustCompile(`\[stdin-bytes=(\d+)\]`)
	gotEcho := false
	for _, d := range expertDirs {
		out, err := os.ReadFile(filepath.Join(expertsRoot, d.Name(), "output.md"))
		if err != nil {
			continue
		}
		m := rxN.FindStringSubmatch(string(out))
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n < len(question) {
			t.Errorf("F6: expert %s reported stdin-bytes=%d, want >= %d", d.Name(), n, len(question))
			continue
		}
		gotEcho = true
	}
	if !gotEcho {
		t.Fatalf("F6: no expert output.md contained an [stdin-bytes=N] marker with N >= %d", len(question))
	}
}

// TestF7a_UnknownTopLevelField: a bad config (effort: bogus at root)
// must produce exit 1 and stderr that names the offending field so
// operators can locate it without grepping.
func TestF7a_UnknownTopLevelField(t *testing.T) {
	bin := testBinary(t)
	cwd := t.TempDir()
	bad := strings.Replace(validProfileYAML, "version: 2\n", "version: 2\neffort: bogus\n", 1)
	writeProfile(t, cwd, bad, defaultPrompts())
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"q"},
		env:      map[string]string{envMock: "trivial"},
		cwd:      cwd,
		deadline: 30 * time.Second,
	})
	if res.exitCode != 1 {
		t.Fatalf("F7a: exit %d, want 1\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "effort") {
		t.Fatalf("F7a: stderr should name the bad field 'effort':\n%s", res.stderr)
	}
}

// TestF7b_PromptYAMLFrontmatter: an expert prompt that begins with
// `---\nfoo: bar\n---` is rejected by the loader (frontmatter is a
// v2-reserved syntax). Exit 1 with stderr pointing at the prompt file.
func TestF7b_PromptYAMLFrontmatter(t *testing.T) {
	bin := testBinary(t)
	cwd := t.TempDir()
	prompts := defaultPrompts()
	prompts["independent.md"] = "---\nfoo: bar\n---\nrest of body\n"
	writeProfile(t, cwd, validProfileYAML, prompts)
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"q"},
		env:      map[string]string{envMock: "trivial"},
		cwd:      cwd,
		deadline: 30 * time.Second,
	})
	if res.exitCode != 1 {
		t.Fatalf("F7b: exit %d, want 1\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "independent.md") {
		t.Fatalf("F7b: stderr should point at the prompt file 'independent.md':\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "frontmatter") {
		t.Fatalf("F7b: stderr should mention 'frontmatter':\n%s", res.stderr)
	}
}

// TestF8_AnonymizationConsistency: every (label, real_name) pair in
// rounds[].experts[] must match the top-level anonymization map. This
// gates ADR-0008's deterministic label derivation: a session id deterministically
// produces the same (label → real_name) map, and that map must be the
// only place the orchestrator ever reads label-to-name from.
func TestF8_AnonymizationConsistency(t *testing.T) {
	bin := testBinary(t)
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"hello"},
		env:      map[string]string{envMock: "trivial"},
		deadline: 30 * time.Second,
	})
	if res.exitCode != 0 {
		t.Fatalf("F8: exit %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	v := readVerdict(t, onlySession(t, res.cwd))
	anon, _ := v["anonymization"].(map[string]any)
	if len(anon) == 0 {
		t.Fatalf("F8: anonymization map empty\nverdict:\n%v", v)
	}
	rounds, _ := v["rounds"].([]any)
	if len(rounds) == 0 {
		t.Fatalf("F8: no rounds")
	}
	for i, r := range rounds {
		rm, _ := r.(map[string]any)
		experts, _ := rm["experts"].([]any)
		for j, e := range experts {
			em, _ := e.(map[string]any)
			label, _ := em["label"].(string)
			realName, _ := em["real_name"].(string)
			wantName, ok := anon[label].(string)
			if !ok {
				t.Errorf("F8: rounds[%d].experts[%d].label=%q not in anonymization map", i, j, label)
				continue
			}
			if realName != wantName {
				t.Errorf("F8: rounds[%d].experts[%d] label=%q real_name=%q, anonymization says %q",
					i, j, label, realName, wantName)
			}
		}
	}
}

// TestF9_ForgeryDetection: the forge_fence_r1 mock makes the label-A R1
// expert emit a forged `=== EXPERT: … ===` fence. The engine must mark
// that expert as failed in R1 while other experts proceed; quorum=1 keeps
// the run going through R2 + voting. The session reaches status=ok (or
// no_consensus if the forgery drops cohort to a tie path), and at least
// one expert has participation=failed in R1.
//
// Unit-level forgery coverage lives in pkg/prompt/injection_test.go; this
// test verifies the end-to-end path wires that rejection through.
func TestF9_ForgeryDetection(t *testing.T) {
	bin := testBinary(t)
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"hello"},
		env:      map[string]string{envMock: "forge_fence_r1"},
		deadline: 30 * time.Second,
	})
	// The forgery only drops label A; labels B and C succeed, quorum=1 is
	// met. With default ballots (VOTE: A) and A absent from the active
	// cohort, every ballot is discarded → tie among B and C → exit 2.
	// Accept either status as long as the participation shape is right.
	if res.exitCode != 0 && res.exitCode != 2 {
		t.Fatalf("F9: exit %d, want 0 or 2\nstderr:\n%s", res.exitCode, res.stderr)
	}
	v := readVerdict(t, onlySession(t, res.cwd))
	rounds, _ := v["rounds"].([]any)
	if len(rounds) == 0 {
		t.Fatalf("F9: no rounds in verdict")
	}
	r0, _ := rounds[0].(map[string]any)
	experts, _ := r0["experts"].([]any)
	var failedLabel string
	for _, e := range experts {
		em, _ := e.(map[string]any)
		if em["participation"] == "failed" {
			failedLabel, _ = em["label"].(string)
			break
		}
	}
	if failedLabel == "" {
		t.Fatalf("F9: no R1 expert with participation=failed\nexperts:\n%v", experts)
	}
	if failedLabel != "A" {
		t.Errorf("F9: failed label = %q, want A (the forger)", failedLabel)
	}
}

// TestF12_VoteOutcomeExactlyOne: voting.winner and voting.tied_candidates
// are mutually exclusive; exactly one is populated for any run that
// reaches the voting stage. Runs both the winner path (trivial mock) and
// the tie path (self_vote_tie mock) and asserts the invariant on each
// verdict.json.
func TestF12_VoteOutcomeExactlyOne(t *testing.T) {
	bin := testBinary(t)

	cases := []struct {
		name           string
		mock           string
		wantExitCode   int
		wantWinner     bool
		wantTieNonZero bool
	}{
		{name: "winner", mock: "trivial", wantExitCode: 0, wantWinner: true},
		{name: "tie", mock: "self_vote_tie", wantExitCode: 2, wantTieNonZero: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runCouncil(t, runOpts{
				binary:   bin,
				args:     []string{"hello"},
				env:      map[string]string{envMock: tc.mock},
				deadline: 30 * time.Second,
			})
			if res.exitCode != tc.wantExitCode {
				t.Fatalf("F12 %s: exit %d, want %d\nstderr:\n%s",
					tc.name, res.exitCode, tc.wantExitCode, res.stderr)
			}
			v := readVerdict(t, onlySession(t, res.cwd))
			voting, _ := v["voting"].(map[string]any)
			if voting == nil {
				t.Fatalf("F12 %s: voting missing\nverdict:\n%v", tc.name, v)
			}
			winner, _ := voting["winner"].(string)
			tied, _ := voting["tied_candidates"].([]any)
			hasWinner := winner != ""
			hasTie := len(tied) > 0
			if hasWinner == hasTie {
				t.Fatalf("F12 %s: winner=%q tied=%v — exactly one must be populated",
					tc.name, winner, tied)
			}
			if tc.wantWinner && !hasWinner {
				t.Fatalf("F12 %s: want winner, got tie %v", tc.name, tied)
			}
			if tc.wantTieNonZero && !hasTie {
				t.Fatalf("F12 %s: want tied candidates, got winner=%q", tc.name, winner)
			}

			if tc.wantWinner {
				// output.md exists for the unique-winner path.
				sessPath := onlySession(t, res.cwd)
				if _, err := os.Stat(filepath.Join(sessPath, "output.md")); err != nil {
					t.Errorf("F12 winner: output.md missing: %v", err)
				}
			}
			if tc.wantTieNonZero {
				// One output-<label>.md per tied candidate.
				labels := make([]string, 0, len(tied))
				for _, l := range tied {
					s, _ := l.(string)
					labels = append(labels, s)
				}
				sort.Strings(labels)
				sessPath := onlySession(t, res.cwd)
				for _, l := range labels {
					p := filepath.Join(sessPath, "output-"+l+".md")
					if _, err := os.Stat(p); err != nil {
						t.Errorf("F12 tie: expected %s: %v", p, err)
					}
				}
			}
		})
	}
}

// TestF_ResumeAfterSIGINT: drives the resume subcommand end-to-end. First
// runs with slow_after_r1 so R1 completes cleanly and R2 blocks forever,
// then SIGINTs mid-R2. Re-invokes `council resume` with a trivial mock
// to finish the remaining stages. Verdict.json status ends up "ok" and
// the root .done marker lands on disk. This is the SIGINT + finality-
// based-resume predicate from D14 / F6 (ADR-0008).
func TestF_ResumeAfterSIGINT(t *testing.T) {
	bin := testBinary(t)
	cwd := t.TempDir()

	// First run: slow_after_r1 so R1 writes .done markers, R2 blocks.
	cmd := exec.Command(bin, "resume-me")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), envMock+"=slow_after_r1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start slow_after_r1: %v", err)
	}
	time.AfterFunc(3*time.Second, func() { _ = cmd.Process.Signal(syscall.SIGINT) })
	time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	exitCode := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("wait slow_after_r1: %v", err)
	}
	if exitCode != 130 {
		t.Fatalf("slow_after_r1 exit %d, want 130", exitCode)
	}
	sess := onlySession(t, cwd)
	if v := readVerdict(t, sess); v["status"] != "interrupted" {
		t.Fatalf("interim status = %v, want interrupted", v["status"])
	}
	if _, err := os.Stat(filepath.Join(sess, ".done")); err == nil {
		t.Fatalf(".done must not exist after SIGINT")
	}
	// At least one R1 expert .done must exist so resume has progress to
	// carry forward — otherwise FindIncomplete would skip this session.
	r1 := filepath.Join(sess, "rounds", "1", "experts")
	r1Dirs, _ := os.ReadDir(r1)
	r1Done := 0
	for _, d := range r1Dirs {
		if _, err := os.Stat(filepath.Join(r1, d.Name(), ".done")); err == nil {
			r1Done++
		}
	}
	if r1Done == 0 {
		t.Fatalf("no R1 .done markers after SIGINT; FindIncomplete will skip")
	}

	// Resume with trivial mock; finishes the remaining stages.
	res := runCouncil(t, runOpts{
		binary:   bin,
		args:     []string{"resume"},
		env:      map[string]string{envMock: "trivial"},
		cwd:      cwd,
		deadline: 30 * time.Second,
	})
	if res.exitCode != 0 {
		t.Fatalf("resume: exit %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	v := readVerdict(t, sess)
	if v["status"] != "ok" {
		t.Fatalf("post-resume status = %v, want ok", v["status"])
	}
	if _, err := os.Stat(filepath.Join(sess, ".done")); err != nil {
		t.Fatalf("post-resume .done missing: %v", err)
	}
}
