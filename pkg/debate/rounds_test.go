package debate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/session"
)

// testExec is a per-test stub Executor. fn is invoked for every Execute call
// and decides the attempt outcome; the stub owns the stdout/stderr file writes
// so RunRound1's post-exec logic (read stdout, check forgery, touch .done) has
// real bytes to operate on.
type testExec struct {
	name string
	fn   func(ctx context.Context, req executor.Request, attempt int) (body string, err error)

	mu       sync.Mutex
	attempts map[string]int
}

func (t *testExec) Name() string { return t.name }

func (t *testExec) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	t.mu.Lock()
	if t.attempts == nil {
		t.attempts = map[string]int{}
	}
	n := t.attempts[req.StdoutFile]
	t.attempts[req.StdoutFile] = n + 1
	t.mu.Unlock()

	start := time.Now()
	body, err := t.fn(ctx, req, n)
	dur := time.Since(start)
	if err != nil {
		_ = os.WriteFile(req.StderrFile, []byte(err.Error()+"\n"), 0o644)
		return executor.Response{ExitCode: 1, Duration: dur}, err
	}
	if werr := os.WriteFile(req.StdoutFile, []byte(body), 0o644); werr != nil {
		return executor.Response{ExitCode: 0, Duration: dur}, werr
	}
	return executor.Response{ExitCode: 0, Duration: dur}, nil
}

const testExecName = "test-debate-exec"

// testProfile builds a minimal v2 Profile with 3 experts bound to testExec.
// testR2PromptBody is the peer-aware role body used by every R2 test. It is
// intentionally distinct from the R1 per-role bodies so prompt assertions can
// tell which one landed in the subprocess input.
const testR2PromptBody = "you are a peer-aware expert; prior-round consensus is NOT ground truth"

func testProfile(t *testing.T) *config.Profile {
	t.Helper()
	return &config.Profile{
		Version: 2,
		Name:    "debate-test",
		Experts: []config.RoleConfig{
			{Name: "alpha", Executor: testExecName, Model: "sonnet", PromptBody: "you are alpha", Timeout: 5 * time.Second},
			{Name: "bravo", Executor: testExecName, Model: "sonnet", PromptBody: "you are bravo", Timeout: 5 * time.Second},
			{Name: "charlie", Executor: testExecName, Model: "sonnet", PromptBody: "you are charlie", Timeout: 5 * time.Second},
		},
		Quorum:       1,
		MaxRetries:   1,
		Rounds:       2,
		Round2Prompt: config.PromptSource{Body: testR2PromptBody},
		Voting: config.VotingConfig{
			BallotPromptBody: "VOTE: <label>\n",
			Timeout:          5 * time.Second,
		},
	}
}

// setupRoundTest materialises a real session folder, assigns labels, and
// returns the pieces RunRound1 needs. The caller supplies its own Executor
// (registered under testExecName) since test behavior differs per case.
func setupRoundTest(t *testing.T, nonce string, exec executor.Executor) (*session.Session, *config.Profile, []LabeledExpert) {
	t.Helper()
	executor.ResetForTest()
	executor.Register(exec)

	cwd := t.TempDir()
	id := session.NewID(time.Now())
	prof := testProfile(t)
	s, err := session.Create(cwd, id, prof, nonce, "what is 2+2?")
	if err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	experts := make([]Expert, len(prof.Experts))
	for i, r := range prof.Experts {
		experts[i] = Expert{Name: r.Name}
	}
	mapping, err := AssignLabels(s.ID, experts)
	if err != nil {
		t.Fatalf("AssignLabels: %v", err)
	}
	labeled := make([]LabeledExpert, 0, len(mapping))
	for label, realName := range mapping {
		for _, role := range prof.Experts {
			if role.Name == realName {
				labeled = append(labeled, LabeledExpert{Label: label, Role: role})
				break
			}
		}
	}
	return s, prof, labeled
}

// byLabel looks up a RoundOutput slot for a given label. Returns nil if not
// present — tests that expect a label to be in the result treat nil as fatal.
func byLabel(outs []RoundOutput, label string) *RoundOutput {
	for i := range outs {
		if outs[i].Label == label {
			return &outs[i]
		}
	}
	return nil
}

func TestRunRound1_AllSucceed(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "answer from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "abcdef0123456789", exec)

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "what is 2+2?")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}
	if len(outs) != 3 {
		t.Fatalf("len(outs) = %d, want 3", len(outs))
	}
	for _, o := range outs {
		if o.Participation != "ok" {
			t.Errorf("label %s participation = %q, want ok", o.Label, o.Participation)
		}
		if !strings.Contains(o.Body, "answer from "+o.Label) {
			t.Errorf("label %s body = %q, missing answer marker", o.Label, o.Body)
		}
		outPath := filepath.Join(s.RoundExpertDir(1, o.Label), "output.md")
		if _, err := os.Stat(outPath); err != nil {
			t.Errorf("label %s: output.md missing: %v", o.Label, err)
		}
		donePath := filepath.Join(s.RoundExpertDir(1, o.Label), ".done")
		if _, err := os.Stat(donePath); err != nil {
			t.Errorf("label %s: .done missing: %v", o.Label, err)
		}
	}
}

func TestRunRound1_ResultsOrderedByLabel(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "ok\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "00112233aabbccdd", exec)
	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}
	for i := 1; i < len(outs); i++ {
		if outs[i-1].Label >= outs[i].Label {
			t.Fatalf("outs not sorted by label: %v", labelsOf(outs))
		}
	}
}

func labelsOf(outs []RoundOutput) []string {
	s := make([]string, len(outs))
	for i, o := range outs {
		s[i] = o.Label
	}
	return s
}

func TestRunRound1_RetryExhaustedMarksFailed(t *testing.T) {
	// "C" always fails; with MaxRetries=1 the runner makes 2 attempts and
	// then gives up. Other labels succeed.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "C" {
				return "", errors.New("simulated subprocess failure")
			}
			return "answer from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "0101010101010101", exec)

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1 (quorum=1, 2 survivors): unexpected err %v", err)
	}
	c := byLabel(outs, "C")
	if c == nil {
		t.Fatalf("label C missing from outs")
	}
	if c.Participation != "failed" {
		t.Fatalf("C participation = %q, want failed", c.Participation)
	}
	// Retry budget of 1 means 2 total attempts; the stub counted them.
	exec.mu.Lock()
	attempts := exec.attempts[filepath.Join(s.RoundExpertDir(1, "C"), "output.md")]
	exec.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("C attempts = %d, want 2 (1 initial + 1 retry)", attempts)
	}
	donePath := filepath.Join(s.RoundExpertDir(1, "C"), ".done")
	if _, err := os.Stat(donePath); !os.IsNotExist(err) {
		t.Fatalf("C .done should not exist on failure, stat err = %v", err)
	}
	// A and B succeeded.
	for _, label := range []string{"A", "B"} {
		o := byLabel(outs, label)
		if o == nil {
			t.Fatalf("label %s missing", label)
		}
		if o.Participation != "ok" {
			t.Errorf("label %s participation = %q, want ok", label, o.Participation)
		}
	}
}

func TestRunRound1_NonceLeakageMarksFailed(t *testing.T) {
	nonce := "deadbeefcafe0001"
	// "C" echoes the session nonce in its output; CheckForgery must reject.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "C" {
				return "here is the secret: " + nonce + " extra\n", nil
			}
			return "clean answer from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, nonce, exec)

	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}
	c := byLabel(outs, "C")
	if c == nil {
		t.Fatalf("label C missing from outs")
	}
	if c.Participation != "failed" {
		t.Fatalf("C participation = %q, want failed", c.Participation)
	}
	// output.md was written by the executor (we preserve it for debugging);
	// .done must NOT be present since forgery short-circuits finalization.
	donePath := filepath.Join(s.RoundExpertDir(1, "C"), ".done")
	if _, err := os.Stat(donePath); !os.IsNotExist(err) {
		t.Fatalf("C .done must not exist after forgery, stat err = %v", err)
	}
	// Body must be empty on failure so the caller cannot accidentally feed
	// the leaked bytes into R2.
	if c.Body != "" {
		t.Errorf("C body must be empty on failure, got %q", c.Body)
	}
}

func TestRunRound1_QuorumFailed(t *testing.T) {
	// All 3 experts fail; quorum=2 (unsatisfiable) surfaces ErrQuorumFailedR1.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "", errors.New("all fail")
		},
	}
	s, _, labeled := setupRoundTest(t, "1111222233334444", exec)
	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     2,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if !errors.Is(err, ErrQuorumFailedR1) {
		t.Fatalf("err = %v, want ErrQuorumFailedR1", err)
	}
	// Outputs returned so the orchestrator can still write verdict.json
	// with the failed experts recorded.
	if len(outs) != 3 {
		t.Fatalf("len(outs) = %d, want 3", len(outs))
	}
	for _, o := range outs {
		if o.Participation != "failed" {
			t.Errorf("label %s participation = %q, want failed", o.Label, o.Participation)
		}
	}
}

func TestRunRound1_QuorumMetWithSurvivors(t *testing.T) {
	// Quorum=1; one failure should still satisfy quorum.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "B" {
				return "", errors.New("B fails")
			}
			return "ok from " + label + "\n", nil
		},
	}
	s, _, labeled := setupRoundTest(t, "aaaabbbbccccdddd", exec)
	outs, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     1,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1 quorum=1 with 2 survivors: %v", err)
	}
	survivors := 0
	for _, o := range outs {
		if o.Participation == "ok" {
			survivors++
		}
	}
	if survivors != 2 {
		t.Fatalf("survivors = %d, want 2", survivors)
	}
}

func TestRunRound1_PromptFileWritten(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "ok\n", nil
		},
	}
	s, _, labeled := setupRoundTest(t, "5555666677778888", exec)
	_, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     1,
		MaxRetries: 0,
		Nonce:      s.Nonce,
	}, "what is 2+2?")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}
	for _, ex := range labeled {
		promptPath := filepath.Join(s.RoundExpertDir(1, ex.Label), "prompt.md")
		body, err := os.ReadFile(promptPath)
		if err != nil {
			t.Fatalf("read prompt.md for %s: %v", ex.Label, err)
		}
		if !strings.Contains(string(body), "what is 2+2?") {
			t.Errorf("%s prompt.md missing question: %q", ex.Label, body)
		}
		if !strings.Contains(string(body), ex.Role.PromptBody) {
			t.Errorf("%s prompt.md missing role body %q: %q", ex.Label, ex.Role.PromptBody, body)
		}
	}
}

func TestRunRound1_GrantsWebTools(t *testing.T) {
	// F13: every R1 expert subprocess must be spawned with the hardcoded
	// ADR-0010 D17 allow-list (WebSearch + WebFetch) and the
	// bypassPermissions mode. No profile gating — the values come from
	// package-level constants in pkg/debate so a future profile field
	// cannot accidentally downgrade the tools.
	var (
		mu      sync.Mutex
		seen    = map[string]executor.Request{}
	)
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			mu.Lock()
			seen[label] = req
			mu.Unlock()
			return "ok from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "1313131313131313", exec)
	_, err := RunRound1(context.Background(), RoundConfig{
		Session:    s,
		Experts:    labeled,
		Quorum:     prof.Quorum,
		MaxRetries: prof.MaxRetries,
		Nonce:      s.Nonce,
	}, "q")
	if err != nil {
		t.Fatalf("RunRound1: %v", err)
	}
	if len(seen) != len(labeled) {
		t.Fatalf("captured %d spawns, want %d", len(seen), len(labeled))
	}
	wantTools := []string{"WebSearch", "WebFetch"}
	for _, ex := range labeled {
		req, ok := seen[ex.Label]
		if !ok {
			t.Fatalf("no spawn recorded for label %s", ex.Label)
		}
		if !equalStrSlice(req.AllowedTools, wantTools) {
			t.Errorf("%s AllowedTools = %#v, want %#v", ex.Label, req.AllowedTools, wantTools)
		}
		if req.PermissionMode != "bypassPermissions" {
			t.Errorf("%s PermissionMode = %q, want bypassPermissions", ex.Label, req.PermissionMode)
		}
	}
}

func TestRunRound2_GrantsWebTools(t *testing.T) {
	// F13 R2 counterpart: every R2 spawn must carry the same hardcoded
	// allow-list + permission mode as R1. Experts that failed R1 are not
	// invoked in R2, so a drop there does not cancel this invariant — we
	// assert on the experts that actually did run.
	var (
		mu   sync.Mutex
		seen = map[string]executor.Request{}
	)
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			mu.Lock()
			seen[label] = req
			mu.Unlock()
			return "r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "1414141414141414", exec)
	r1 := r1For(labeled)
	_, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	if len(seen) != len(labeled) {
		t.Fatalf("captured %d R2 spawns, want %d", len(seen), len(labeled))
	}
	wantTools := []string{"WebSearch", "WebFetch"}
	for _, ex := range labeled {
		req, ok := seen[ex.Label]
		if !ok {
			t.Fatalf("no R2 spawn recorded for label %s", ex.Label)
		}
		if !equalStrSlice(req.AllowedTools, wantTools) {
			t.Errorf("%s R2 AllowedTools = %#v, want %#v", ex.Label, req.AllowedTools, wantTools)
		}
		if req.PermissionMode != "bypassPermissions" {
			t.Errorf("%s R2 PermissionMode = %q, want bypassPermissions", ex.Label, req.PermissionMode)
		}
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunRound1_NilSession(t *testing.T) {
	_, err := RunRound1(context.Background(), RoundConfig{
		Session:    nil,
		Experts:    []LabeledExpert{{Label: "A", Role: config.RoleConfig{Name: "x"}}},
		Quorum:     1,
		MaxRetries: 0,
	}, "q")
	if err == nil {
		t.Fatalf("expected error for nil Session")
	}
}

// r1For builds a []RoundOutput matching the given labeled experts, with
// every entry marked "ok" and a body of the form "r1 from <label>". Used
// as the R1 input for R2 tests where R1 is treated as a done input.
func r1For(labeled []LabeledExpert) []RoundOutput {
	outs := make([]RoundOutput, len(labeled))
	for i, ex := range labeled {
		outs[i] = RoundOutput{
			Label:         ex.Label,
			Name:          ex.Role.Name,
			Participation: "ok",
			Body:          "r1 from " + ex.Label + "\n",
		}
	}
	return outs
}

func TestBuildPeerAggregate_ExcludesSelfSortsWraps(t *testing.T) {
	nonce := "0123456789abcdef"
	outs := []RoundOutput{
		{Label: "C", Name: "charlie", Participation: "ok", Body: "c body"},
		{Label: "A", Name: "alpha", Participation: "ok", Body: "a body"},
		{Label: "B", Name: "bravo", Participation: "ok", Body: "b body"},
	}
	got := buildPeerAggregate("B", outs, nonce)
	// B is excluded; A and C remain, alphabetical; each wrapped; sections
	// separated by a blank line.
	want := "=== EXPERT: A [nonce-" + nonce + "] ===\na body\n=== END EXPERT: A [nonce-" + nonce + "] ===" +
		"\n\n" +
		"=== EXPERT: C [nonce-" + nonce + "] ===\nc body\n=== END EXPERT: C [nonce-" + nonce + "] ==="
	if got != want {
		t.Fatalf("aggregate mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildPeerAggregate_SkipsFailed(t *testing.T) {
	nonce := "aaaabbbbccccdddd"
	outs := []RoundOutput{
		{Label: "A", Participation: "ok", Body: "a body"},
		{Label: "B", Participation: "failed", Body: ""},
		{Label: "C", Participation: "ok", Body: "c body"},
	}
	got := buildPeerAggregate("A", outs, nonce)
	// A is self; B is failed (skip); C remains.
	want := "=== EXPERT: C [nonce-" + nonce + "] ===\nc body\n=== END EXPERT: C [nonce-" + nonce + "] ==="
	if got != want {
		t.Fatalf("aggregate mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildPeerAggregate_AllFailed(t *testing.T) {
	outs := []RoundOutput{
		{Label: "A", Participation: "failed"},
		{Label: "B", Participation: "failed"},
	}
	got := buildPeerAggregate("A", outs, "ffffeeeeddddcccc")
	if got != "" {
		t.Fatalf("expected empty aggregate for all-failed peers, got %q", got)
	}
}

func TestRunRound2_AllSucceed(t *testing.T) {
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "1212121212121212", exec)
	r1 := r1For(labeled)

	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "what is 2+2?", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	if len(outs) != 3 {
		t.Fatalf("len(outs) = %d, want 3", len(outs))
	}
	for _, o := range outs {
		if o.Participation != "ok" {
			t.Errorf("label %s participation = %q, want ok", o.Label, o.Participation)
		}
		if !strings.Contains(o.Body, "r2 from "+o.Label) {
			t.Errorf("label %s body = %q, want r2 marker", o.Label, o.Body)
		}
		outPath := filepath.Join(s.RoundExpertDir(2, o.Label), "output.md")
		if _, err := os.Stat(outPath); err != nil {
			t.Errorf("label %s: R2 output.md missing: %v", o.Label, err)
		}
		donePath := filepath.Join(s.RoundExpertDir(2, o.Label), ".done")
		if _, err := os.Stat(donePath); err != nil {
			t.Errorf("label %s: R2 .done missing: %v", o.Label, err)
		}
	}
}

func TestRunRound2_PromptContainsPeerAggregate(t *testing.T) {
	// Capture the prompt each expert receives; assert it includes the
	// two other experts' R1 outputs wrapped with nonce, and excludes
	// the expert's own R1 output.
	nonce := "cafebabe12345678"
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			// Persist the prompt so the test can read it back.
			_ = os.WriteFile(filepath.Join(filepath.Dir(req.StdoutFile), "prompt-seen.txt"), []byte(req.Prompt), 0o644)
			return "r2 " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, nonce, exec)
	r1 := r1For(labeled)
	_, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	for _, ex := range labeled {
		seen, err := os.ReadFile(filepath.Join(s.RoundExpertDir(2, ex.Label), "prompt-seen.txt"))
		if err != nil {
			t.Fatalf("read prompt-seen for %s: %v", ex.Label, err)
		}
		body := string(seen)
		// Must include each OTHER expert's label fenced with nonce, in
		// alphabetical order, with the R1 body.
		other := []string{}
		for _, o := range labeled {
			if o.Label != ex.Label {
				other = append(other, o.Label)
			}
		}
		if other[0] > other[1] {
			other[0], other[1] = other[1], other[0]
		}
		for _, peer := range other {
			fence := "=== EXPERT: " + peer + " [nonce-" + nonce + "] ==="
			if !strings.Contains(body, fence) {
				t.Errorf("%s prompt missing peer fence %q", ex.Label, fence)
			}
			if !strings.Contains(body, "r1 from "+peer) {
				t.Errorf("%s prompt missing peer body for %s", ex.Label, peer)
			}
		}
		// Must NOT include own fence (anti self-echo).
		selfFence := "=== EXPERT: " + ex.Label + " [nonce-" + nonce + "] ==="
		if strings.Contains(body, selfFence) {
			t.Errorf("%s prompt incorrectly includes its own fence %q", ex.Label, selfFence)
		}
		// R2 must use the peer-aware prompt, NOT the per-expert R1 role
		// body (design §3.4: `[peer-aware role prompt]` replaces the
		// independent prompt in round 2).
		if !strings.Contains(body, testR2PromptBody) {
			t.Errorf("%s prompt missing peer-aware R2 prompt", ex.Label)
		}
		if strings.Contains(body, ex.Role.PromptBody) {
			t.Errorf("%s prompt still embeds R1 role body %q — R2 should use peer-aware prompt only", ex.Label, ex.Role.PromptBody)
		}
		if !strings.Contains(body, "=== USER QUESTION") {
			t.Errorf("%s prompt missing user-question block", ex.Label)
		}
		// Peer aggregate must be alphabetically ordered within the prompt.
		idx0 := strings.Index(body, "=== EXPERT: "+other[0])
		idx1 := strings.Index(body, "=== EXPERT: "+other[1])
		if idx0 < 0 || idx1 < 0 || idx0 >= idx1 {
			t.Errorf("%s peer order wrong: %s at %d, %s at %d", ex.Label, other[0], idx0, other[1], idx1)
		}
	}
}

func TestRunRound2_CarryForwardOnFailure(t *testing.T) {
	// "C" always fails in R2; its R1 body must be copied forward.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "C" {
				return "", errors.New("C flaky in R2")
			}
			return "r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "3333444455556666", exec)
	r1 := r1For(labeled)
	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	c := byLabel(outs, "C")
	if c == nil {
		t.Fatalf("C missing from outs")
	}
	if c.Participation != "carried" {
		t.Fatalf("C participation = %q, want carried", c.Participation)
	}
	// Body is the R1 body copied forward.
	if !strings.Contains(c.Body, "r1 from C") {
		t.Errorf("C body after carry-forward = %q, want R1 content", c.Body)
	}
	// output.md in R2 dir must hold R1 bytes.
	outPath := filepath.Join(s.RoundExpertDir(2, "C"), "output.md")
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read R2 output.md for C: %v", err)
	}
	if !strings.Contains(string(body), "r1 from C") {
		t.Errorf("R2 output.md for C = %q, want carried R1 bytes", body)
	}
	// A and B should be ok.
	for _, label := range []string{"A", "B"} {
		o := byLabel(outs, label)
		if o.Participation != "ok" {
			t.Errorf("%s participation = %q, want ok", label, o.Participation)
		}
	}
}

func TestRunRound2_CarryForwardOnForgery(t *testing.T) {
	// "A" echoes the session nonce back — R2 must reject, then
	// carry-forward the R1 body.
	nonce := "deadbeefdeadbeef"
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "A" {
				return "leak: " + nonce + "\n", nil
			}
			return "clean r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, nonce, exec)
	r1 := r1For(labeled)
	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	a := byLabel(outs, "A")
	if a.Participation != "carried" {
		t.Fatalf("A participation = %q, want carried (forgery -> carry-forward)", a.Participation)
	}
	if !strings.Contains(a.Body, "r1 from A") {
		t.Errorf("A body = %q, want R1 bytes after forgery carry-forward", a.Body)
	}
	// Forensic: the rejected R2 output must be preserved next to the
	// carry-forward output.md so investigators can inspect the LLM's
	// injection attempt. runExpertR1 applies the same retention policy on
	// its forgery path; R2 must not silently destroy this evidence.
	rejectedPath := filepath.Join(s.RoundExpertDir(2, "A"), "output.rejected.md")
	rejected, rerr := os.ReadFile(rejectedPath)
	if rerr != nil {
		t.Fatalf("read output.rejected.md: %v", rerr)
	}
	if !strings.Contains(string(rejected), nonce) {
		t.Errorf("output.rejected.md = %q, want preserved forged bytes containing nonce %q", rejected, nonce)
	}
}

func TestRunRound2_R1DroppedStaysFailed(t *testing.T) {
	// "B" failed R1 (dropped). In R2, B must remain "failed" and never be
	// invoked; A and C run R2 with a peer aggregate that excludes B.
	r1 := []RoundOutput{}
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			if label == "B" {
				// Guard: we should never invoke B in R2.
				t.Errorf("B was invoked in R2 despite R1 failure")
			}
			return "r2 from " + label + "\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "7070707070707070", exec)
	// Build r1 matching labeled, but with B failed.
	for _, ex := range labeled {
		r := RoundOutput{Label: ex.Label, Name: ex.Role.Name}
		if ex.Label == "B" {
			r.Participation = "failed"
		} else {
			r.Participation = "ok"
			r.Body = "r1 from " + ex.Label + "\n"
		}
		r1 = append(r1, r)
	}
	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	b := byLabel(outs, "B")
	if b.Participation != "failed" {
		t.Errorf("B participation = %q, want failed (R1-dropped experts stay failed)", b.Participation)
	}
	for _, label := range []string{"A", "C"} {
		o := byLabel(outs, label)
		if o.Participation != "ok" {
			t.Errorf("%s participation = %q, want ok", label, o.Participation)
		}
	}
	// Aggregate file must hold A and C entries, not B.
	aggPath := filepath.Join(s.Path, "rounds", "2", "aggregate.md")
	agg, err := os.ReadFile(aggPath)
	if err != nil {
		t.Fatalf("read aggregate: %v", err)
	}
	for _, label := range []string{"A", "C"} {
		if !strings.Contains(string(agg), "=== EXPERT: "+label+" [nonce-") {
			t.Errorf("aggregate missing %s fence", label)
		}
	}
	if strings.Contains(string(agg), "=== EXPERT: B [nonce-") {
		t.Errorf("aggregate contains B fence despite R1 drop")
	}
}

func TestRunRound2_InterruptSkipsCarryForward(t *testing.T) {
	// SIGINT mid-R2: ctx is cancelled, runWithFailRetry returns
	// context.Canceled. The R2 runner must NOT write .done/.carried
	// for the interrupted expert — otherwise `council resume` would
	// short-circuit R2 via readCompletedStage and silently replace
	// the missing R2 body with the R1 body.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "should not be called", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, "cafebabecafebabe", exec)
	r1 := r1For(labeled)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE RunRound2 so every expert sees ctx.Err() != nil

	outs, err := RunRound2(ctx, RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	// Quorum failure is expected: every expert is "failed" (not "carried"),
	// so survivors = 0 < quorum.
	if !errors.Is(err, ErrQuorumFailedR2) {
		t.Fatalf("err = %v, want ErrQuorumFailedR2", err)
	}
	for _, label := range []string{"A", "B", "C"} {
		o := byLabel(outs, label)
		if o == nil {
			t.Fatalf("%s missing from outs", label)
		}
		if o.Participation != "failed" {
			t.Errorf("%s participation = %q, want failed (ctx cancelled, carry-forward must be skipped)", label, o.Participation)
		}
		dir := s.RoundExpertDir(2, label)
		if _, err := os.Stat(filepath.Join(dir, ".done")); err == nil {
			t.Errorf("%s .done exists after interrupt — resume will wrongly short-circuit R2", label)
		}
		if _, err := os.Stat(filepath.Join(dir, ".carried")); err == nil {
			t.Errorf("%s .carried exists after interrupt — carry-forward should be skipped on ctx cancel", label)
		}
	}
}

func TestRunRound2_QuorumFailed(t *testing.T) {
	// All 3 fail in R2 — but carry-forward means all 3 are "carried".
	// To force quorum failure, start with 2 R1-dropped experts and one
	// R2 failure that still carries forward its R1. Quorum=2 with 1
	// surviving participant → ErrQuorumFailedR2.
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			return "", errors.New("everyone fails R2")
		},
	}
	s, _, labeled := setupRoundTest(t, "8989898989898989", exec)
	// R1: only A survived; B and C failed R1.
	r1 := []RoundOutput{}
	for _, ex := range labeled {
		r := RoundOutput{Label: ex.Label, Name: ex.Role.Name}
		if ex.Label == "A" {
			r.Participation = "ok"
			r.Body = "r1 from A\n"
		} else {
			r.Participation = "failed"
		}
		r1 = append(r1, r)
	}
	outs, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       2,
		MaxRetries:   0,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if !errors.Is(err, ErrQuorumFailedR2) {
		t.Fatalf("err = %v, want ErrQuorumFailedR2", err)
	}
	if len(outs) != 3 {
		t.Fatalf("len(outs) = %d, want 3", len(outs))
	}
	// A should be carried (R2 failed but R1 existed).
	a := byLabel(outs, "A")
	if a.Participation != "carried" {
		t.Errorf("A participation = %q, want carried", a.Participation)
	}
}

func TestRunRound2_GlobalAggregateWrittenOnce(t *testing.T) {
	nonce := "abababababababab"
	exec := &testExec{
		name: testExecName,
		fn: func(ctx context.Context, req executor.Request, _ int) (string, error) {
			label := filepath.Base(filepath.Dir(req.StdoutFile))
			return "R2/" + label + "/body\n", nil
		},
	}
	s, prof, labeled := setupRoundTest(t, nonce, exec)
	r1 := r1For(labeled)
	_, err := RunRound2(context.Background(), RoundConfig{
		Session:      s,
		Experts:      labeled,
		Quorum:       prof.Quorum,
		MaxRetries:   prof.MaxRetries,
		Nonce:        s.Nonce,
		R2PromptBody: testR2PromptBody,
	}, "q", r1)
	if err != nil {
		t.Fatalf("RunRound2: %v", err)
	}
	aggPath := filepath.Join(s.Path, "rounds", "2", "aggregate.md")
	body, err := os.ReadFile(aggPath)
	if err != nil {
		t.Fatalf("read aggregate: %v", err)
	}
	// Alphabetical order of fences in the file.
	idxA := strings.Index(string(body), "=== EXPERT: A [nonce-")
	idxB := strings.Index(string(body), "=== EXPERT: B [nonce-")
	idxC := strings.Index(string(body), "=== EXPERT: C [nonce-")
	if idxA < 0 || idxB < 0 || idxC < 0 {
		t.Fatalf("aggregate missing a fence: %s", body)
	}
	if !(idxA < idxB && idxB < idxC) {
		t.Fatalf("aggregate not alphabetically ordered: A=%d B=%d C=%d", idxA, idxB, idxC)
	}
	// Each R2 body must be present.
	for _, label := range []string{"A", "B", "C"} {
		if !strings.Contains(string(body), "R2/"+label+"/body") {
			t.Errorf("aggregate missing R2 body for %s", label)
		}
	}
	// Nonce must appear on each fence line.
	if strings.Count(string(body), "[nonce-"+nonce+"]") < 6 {
		t.Errorf("aggregate missing nonces on all fences:\n%s", body)
	}
}

func TestRunRound2_NilSession(t *testing.T) {
	_, err := RunRound2(context.Background(), RoundConfig{
		Session:    nil,
		Experts:    []LabeledExpert{{Label: "A", Role: config.RoleConfig{Name: "x"}}},
		Quorum:     1,
		MaxRetries: 0,
	}, "q", nil)
	if err == nil {
		t.Fatalf("expected error for nil Session")
	}
}
