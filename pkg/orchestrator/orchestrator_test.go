package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
	"github.com/fitz123/council/pkg/runner"
	"github.com/fitz123/council/pkg/session"
)

// stubExec is a test double for executor.Executor. Behavior is driven
// by a callback so each test can inject exactly the success / failure /
// retry sequence it needs. CallCount is atomic so concurrent expert
// goroutines can assert on total invocations.
type stubExec struct {
	name      string
	callCount int64
	// on is called once per Execute. Implementations write the stdout
	// file themselves so the orchestrator can read it afterwards. The
	// ctx is the same one passed to Executor.Execute, so callbacks can
	// observe cancellation to simulate long-running stages.
	on func(ctx context.Context, call int64, req executor.Request) (executor.Response, error)
}

func (s *stubExec) Name() string { return s.name }
func (s *stubExec) Execute(ctx context.Context, req executor.Request) (executor.Response, error) {
	n := atomic.AddInt64(&s.callCount, 1)
	return s.on(ctx, n, req)
}

// writeOK is a helper stubExec callback that writes a fixed body to the
// request's StdoutFile and returns a successful Response.
func writeOK(body string) func(context.Context, int64, executor.Request) (executor.Response, error) {
	return func(_ context.Context, _ int64, req executor.Request) (executor.Response, error) {
		if err := os.WriteFile(req.StdoutFile, []byte(body), 0o644); err != nil {
			return executor.Response{}, err
		}
		return executor.Response{ExitCode: 0, Duration: 10 * time.Millisecond}, nil
	}
}

// register swaps the global registry for the duration of a test. Using
// t.Cleanup means each test starts with a clean registry regardless of
// test order.
func register(t *testing.T, execs ...executor.Executor) {
	t.Helper()
	executor.ResetForTest()
	for _, e := range execs {
		executor.Register(e)
	}
	t.Cleanup(func() { executor.ResetForTest() })
}

// newTestProfile returns a Profile with one or more experts, all using
// the same executor name and similar prompt bodies. Good enough for
// orchestrator tests — we don't exercise YAML loading here.
func newTestProfile(execName string, experts []string, maxRetries int) *config.Profile {
	p := &config.Profile{
		Version: 1,
		Name:    "test",
		Judge: config.RoleConfig{
			Name:       "judge",
			Executor:   execName,
			Model:      "opus",
			PromptFile: "/tmp/judge.md",
			Timeout:    5 * time.Second,
			PromptBody: "JUDGE ROLE",
		},
		Experts:    make([]config.RoleConfig, len(experts)),
		Quorum:     1,
		MaxRetries: maxRetries,
	}
	for i, name := range experts {
		p.Experts[i] = config.RoleConfig{
			Name:       name,
			Executor:   execName,
			Model:      "sonnet",
			PromptFile: "/tmp/" + name + ".md",
			Timeout:    5 * time.Second,
			PromptBody: "EXPERT: " + name,
		}
	}
	return p
}

// newTestSession creates a real on-disk session folder under t.TempDir().
// Using the real Session (rather than a mock) exercises the same IO
// paths the production binary will use; the orchestrator's contract
// with session is narrow enough that a mock would hide more than reveal.
func newTestSession(t *testing.T, p *config.Profile, question string) *session.Session {
	t.Helper()
	cwd := t.TempDir()
	id := session.NewID(time.Now())
	s, err := session.Create(cwd, id, p, question)
	if err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	return s
}

// TestRun_HappyPath covers the "all experts succeed + judge succeeds"
// case. Asserts verdict.Status == "ok", experts recorded in order,
// answer == judge synthesis, and stderr.log cleaned up from every role.
func TestRun_HappyPath(t *testing.T) {
	stub := &stubExec{
		name: "stub",
		on: func(ctx context.Context, n int64, req executor.Request) (executor.Response, error) {
			body := "EXPERT_OUT"
			if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
				body = "FINAL_ANSWER"
			}
			return writeOK(body)(ctx, n, req)
		},
	}
	register(t, stub)

	p := newTestProfile("stub", []string{"independent", "critic"}, 1)
	s := newTestSession(t, p, "what is 2+2?")

	v, err := Run(context.Background(), p, "what is 2+2?", s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Status != "ok" {
		t.Errorf("status = %q, want ok", v.Status)
	}
	if v.Answer != "FINAL_ANSWER" {
		t.Errorf("answer = %q, want FINAL_ANSWER", v.Answer)
	}
	if got, want := len(v.Rounds[0].Experts), 2; got != want {
		t.Fatalf("experts len = %d, want %d", got, want)
	}
	for i, want := range []string{"independent", "critic"} {
		e := v.Rounds[0].Experts[i]
		if e.Name != want {
			t.Errorf("experts[%d].Name = %q, want %q", i, e.Name, want)
		}
		if e.Status != "ok" {
			t.Errorf("experts[%d].Status = %q, want ok", i, e.Status)
		}
		if e.Retries != 0 {
			t.Errorf("experts[%d].Retries = %d, want 0", i, e.Retries)
		}
	}

	// F4 precondition: stderr.log is absent after a successful run.
	for _, name := range []string{"independent", "critic"} {
		p := filepath.Join(s.ExpertDir(name), "stderr.log")
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s absent on success, got err=%v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.JudgeDir(), ".done")); err != nil {
		t.Errorf("judge .done missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Path, "verdict.json")); err != nil {
		t.Errorf("verdict.json missing: %v", err)
	}
}

// TestRun_FailOnceThenOk is F4 from design §16: first expert attempt
// fails, retry succeeds → verdict records retries == 1.
func TestRun_FailOnceThenOk(t *testing.T) {
	var expertCall int64
	stub := &stubExec{
		name: "stub",
		on: func(ctx context.Context, n int64, req executor.Request) (executor.Response, error) {
			if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
				return writeOK("SYNTHESIS")(ctx, n, req)
			}
			c := atomic.AddInt64(&expertCall, 1)
			if c == 1 {
				// Simulate a real runner failure by leaving stderr behind.
				_ = os.WriteFile(req.StderrFile, []byte("boom\n"), 0o644)
				return executor.Response{ExitCode: 1, Duration: time.Millisecond}, fmt.Errorf("simulated fail")
			}
			return writeOK("EXPERT_OUT")(ctx, n, req)
		},
	}
	register(t, stub)

	p := newTestProfile("stub", []string{"independent"}, 1)
	s := newTestSession(t, p, "q")

	v, err := Run(context.Background(), p, "q", s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Status != "ok" {
		t.Fatalf("status = %q, want ok", v.Status)
	}
	if got := v.Rounds[0].Experts[0].Retries; got != 1 {
		t.Errorf("retries = %d, want 1", got)
	}
}

// TestRun_QuorumFailed asserts that when survivors < quorum, verdict is
// flipped to "quorum_failed" and ErrQuorumFailed returned. Judge is
// never invoked — asserted via an unexpected-call check in the stub.
func TestRun_QuorumFailed(t *testing.T) {
	stub := &stubExec{
		name: "stub",
		on: func(ctx context.Context, n int64, req executor.Request) (executor.Response, error) {
			if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
				t.Error("judge should NOT run when quorum fails")
				return writeOK("")(ctx, n, req)
			}
			_ = os.WriteFile(req.StderrFile, []byte("down\n"), 0o644)
			return executor.Response{ExitCode: 1}, errors.New("fail")
		},
	}
	register(t, stub)

	// Two experts both fail, quorum=1 → unmet.
	p := newTestProfile("stub", []string{"independent", "critic"}, 0)
	s := newTestSession(t, p, "q")

	v, err := Run(context.Background(), p, "q", s)
	if !errors.Is(err, ErrQuorumFailed) {
		t.Fatalf("Run err = %v, want ErrQuorumFailed", err)
	}
	if v.Status != "quorum_failed" {
		t.Errorf("status = %q, want quorum_failed", v.Status)
	}
	for i, e := range v.Rounds[0].Experts {
		if e.Status != "failed" {
			t.Errorf("experts[%d].Status = %q, want failed", i, e.Status)
		}
	}
	// stderr.log persists on failure (design §7)
	for _, name := range []string{"independent", "critic"} {
		p := filepath.Join(s.ExpertDir(name), "stderr.log")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected stderr.log present on failure at %s: %v", p, err)
		}
	}
}

// TestRun_JudgeFailed: experts succeed, judge fails every attempt (2
// total including the 1 retry) → verdict.status="judge_failed",
// ErrJudgeFailed returned, stderr.log persisted in judge dir.
func TestRun_JudgeFailed(t *testing.T) {
	stub := &stubExec{
		name: "stub",
		on: func(ctx context.Context, n int64, req executor.Request) (executor.Response, error) {
			if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
				_ = os.WriteFile(req.StderrFile, []byte("judge down\n"), 0o644)
				return executor.Response{ExitCode: 1}, errors.New("judge fail")
			}
			return writeOK("EXPERT_OUT")(ctx, n, req)
		},
	}
	register(t, stub)

	p := newTestProfile("stub", []string{"independent"}, 0)
	s := newTestSession(t, p, "q")

	v, err := Run(context.Background(), p, "q", s)
	if !errors.Is(err, ErrJudgeFailed) {
		t.Fatalf("Run err = %v, want ErrJudgeFailed", err)
	}
	if v.Status != "judge_failed" {
		t.Errorf("status = %q, want judge_failed", v.Status)
	}
	if got := v.Rounds[0].Judge.Retries; got != 1 {
		t.Errorf("judge retries = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(s.JudgeDir(), "stderr.log")); err != nil {
		t.Errorf("judge stderr.log missing after failure: %v", err)
	}
}

// TestRun_InterruptPartial is the SIGINT partial-completion case from
// the plan: expert A completes, SIGINT fires before B finishes → A is
// "ok", B is "interrupted", judge section is zero-valued, top-level
// status is "interrupted", and ErrInterrupted is returned.
func TestRun_InterruptPartial(t *testing.T) {
	aDone := make(chan struct{})
	stub := &stubExec{
		name: "stub",
		on: func(ctx context.Context, _ int64, req executor.Request) (executor.Response, error) {
			// B blocks until ctx cancels, mimicking a subprocess still
			// running when SIGINT fires.
			if strings.Contains(req.StdoutFile, "/critic/") {
				<-ctx.Done()
				return executor.Response{ExitCode: 0, Duration: 0}, ctx.Err()
			}
			if strings.Contains(req.StdoutFile, "/independent/") {
				if err := os.WriteFile(req.StdoutFile, []byte("A_OUT"), 0o644); err != nil {
					return executor.Response{}, err
				}
				close(aDone)
				return executor.Response{ExitCode: 0, Duration: time.Millisecond}, nil
			}
			t.Errorf("unexpected stub call for %s", req.StdoutFile)
			return executor.Response{}, nil
		},
	}
	register(t, stub)

	p := newTestProfile("stub", []string{"independent", "critic"}, 0)
	s := newTestSession(t, p, "q")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel after A completes → deterministic expert-state in verdict.
	go func() {
		<-aDone
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	v, err := Run(ctx, p, "q", s)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run err = %v, want ErrInterrupted", err)
	}
	if v.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", v.Status)
	}
	if len(v.Rounds[0].Experts) != 2 {
		t.Fatalf("experts len = %d, want 2", len(v.Rounds[0].Experts))
	}
	if v.Rounds[0].Experts[0].Status != "ok" {
		t.Errorf("experts[0].Status = %q, want ok", v.Rounds[0].Experts[0].Status)
	}
	if v.Rounds[0].Experts[1].Status != "interrupted" {
		t.Errorf("experts[1].Status = %q, want interrupted", v.Rounds[0].Experts[1].Status)
	}
	// Judge section zero-valued — never invoked.
	if v.Rounds[0].Judge.Executor != "" {
		t.Errorf("judge.Executor = %q, want empty on interrupt", v.Rounds[0].Judge.Executor)
	}
	// Verdict written before return (so cmd/council can exit 130 safely).
	if _, err := os.Stat(filepath.Join(s.Path, "verdict.json")); err != nil {
		t.Errorf("verdict.json missing after interrupt: %v", err)
	}
}

// TestRun_SharedExecutePath asserts that both expert and judge dispatch
// through the SAME code path — specifically, through Executor.Execute
// on the same registered instance. This is the architect-review P2
// guarantee ("no duplicated subprocess primitive between expert and
// judge"). A grep-based check is brittle; invoking the orchestrator and
// counting real calls is the real assertion.
func TestRun_SharedExecutePath(t *testing.T) {
	stub := &stubExec{
		name: "stub",
		on: func(ctx context.Context, n int64, req executor.Request) (executor.Response, error) {
			body := "OUT"
			if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
				body = "SYN"
			}
			return writeOK(body)(ctx, n, req)
		},
	}
	register(t, stub)

	p := newTestProfile("stub", []string{"independent"}, 0)
	s := newTestSession(t, p, "q")
	if _, err := Run(context.Background(), p, "q", s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 1 expert + 1 judge = 2 total Execute calls on the single stub.
	if got := atomic.LoadInt64(&stub.callCount); got != 2 {
		t.Errorf("stub.callCount = %d, want 2 (both expert and judge must route through Executor.Execute)", got)
	}
}

// TestRun_VerdictOnDisk asserts verdict.json is flushed on both failure
// paths (quorum_failed, judge_failed). Happy-path coverage is in
// TestRun_HappyPath.
func TestRun_VerdictOnDisk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		judgeOK bool
		want    string
		wantErr error
	}{
		{"quorum_failed", false, "quorum_failed", ErrQuorumFailed},
		{"judge_failed", true, "judge_failed", ErrJudgeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubExec{
				name: "stub",
				on: func(ctx context.Context, n int64, req executor.Request) (executor.Response, error) {
					if strings.HasSuffix(req.StdoutFile, "synthesis.md") {
						_ = os.WriteFile(req.StderrFile, []byte("err"), 0o644)
						return executor.Response{ExitCode: 1}, errors.New("judge fail")
					}
					if !tc.judgeOK {
						_ = os.WriteFile(req.StderrFile, []byte("err"), 0o644)
						return executor.Response{ExitCode: 1}, errors.New("fail")
					}
					return writeOK("OUT")(ctx, n, req)
				},
			}
			register(t, stub)

			p := newTestProfile("stub", []string{"independent"}, 0)
			s := newTestSession(t, p, "q")
			_, err := Run(context.Background(), p, "q", s)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			b, rerr := os.ReadFile(filepath.Join(s.Path, "verdict.json"))
			if rerr != nil {
				t.Fatalf("read verdict.json: %v", rerr)
			}
			if !strings.Contains(string(b), `"status": "`+tc.want+`"`) {
				t.Errorf("verdict.json missing status %q, got: %s", tc.want, string(b))
			}
		})
	}
}

// TestRun_NeverStartedAbsent covers the narrow "ctx cancelled before
// goroutine runs" case. When a goroutine observes ctx.Err()!=nil at the
// top, it must skip recording — resulting experts slice is shorter than
// profile.Experts.
func TestRun_NeverStartedAbsent(t *testing.T) {
	stub := &stubExec{
		name: "stub",
		on: func(_ context.Context, _ int64, _ executor.Request) (executor.Response, error) {
			t.Error("Execute must not be called when ctx is already cancelled")
			return executor.Response{}, errors.New("should not run")
		},
	}
	register(t, stub)

	p := newTestProfile("stub", []string{"a", "b"}, 0)
	s := newTestSession(t, p, "q")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v, err := Run(ctx, p, "q", s)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	if v.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", v.Status)
	}
	if len(v.Rounds[0].Experts) != 0 {
		t.Errorf("experts len = %d, want 0 (all never-started)", len(v.Rounds[0].Experts))
	}
}

// TestRun_UnknownExecutor: an expert names an executor that was never
// registered. Execute never fires; the orchestrator still records the
// expert as failed and the overall run fails quorum.
func TestRun_UnknownExecutor(t *testing.T) {
	// Register only "other"; the profile will ask for "missing".
	register(t, &stubExec{name: "other", on: writeOK("OUT")})

	p := newTestProfile("missing", []string{"x"}, 0)
	s := newTestSession(t, p, "q")

	v, err := Run(context.Background(), p, "q", s)
	if !errors.Is(err, ErrQuorumFailed) {
		t.Fatalf("err = %v, want ErrQuorumFailed (missing executor → expert fails → quorum)", err)
	}
	if v.Rounds[0].Experts[0].Status != "failed" {
		t.Errorf("experts[0].Status = %q, want failed", v.Rounds[0].Experts[0].Status)
	}
}

// TestRunWithFailRetry_Counts verifies retry-counter semantics
// independently of the full pipeline (0 retries on first-try success, N
// on N failures before success, cap at maxRetries).
func TestRunWithFailRetry_Counts(t *testing.T) {
	cases := []struct {
		name        string
		maxRetries  int
		failures    int
		wantRetries int
		wantErr     bool
	}{
		{"success first try", 3, 0, 0, false},
		{"one retry succeeds", 3, 1, 1, false},
		{"exhaust budget", 2, 5, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int64
			stub := &stubExec{
				name: "stub",
				on: func(_ context.Context, _ int64, _ executor.Request) (executor.Response, error) {
					c := atomic.AddInt64(&calls, 1)
					if c <= int64(tc.failures) {
						return executor.Response{ExitCode: 1}, errors.New("fail")
					}
					return executor.Response{ExitCode: 0}, nil
				},
			}
			register(t, stub)

			_, err, retries := runWithFailRetry(context.Background(), "stub", tc.maxRetries, executor.Request{})
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if retries != tc.wantRetries {
				t.Errorf("retries = %d, want %d", retries, tc.wantRetries)
			}
		})
	}
}

// TestRunWithFailRetry_RateLimitShortCircuits asserts that an
// ErrRateLimit surfaced by the runner is NOT retried at the orchestrator
// layer. Rate-limit retries are runner-owned (plan Task 6 / design §10);
// stacking a second orchestrator budget on top of the runner's budget
// would violate that split. The stub returns ErrRateLimit on every call;
// the orchestrator should call Execute exactly once even with a large
// maxRetries budget.
func TestRunWithFailRetry_RateLimitShortCircuits(t *testing.T) {
	var calls int64
	stub := &stubExec{
		name: "stub",
		on: func(_ context.Context, _ int64, _ executor.Request) (executor.Response, error) {
			atomic.AddInt64(&calls, 1)
			return executor.Response{}, runner.ErrRateLimit
		},
	}
	register(t, stub)

	_, err, retries := runWithFailRetry(context.Background(), "stub", 100, executor.Request{})
	if !errors.Is(err, runner.ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0 (orchestrator must not retry rate-limits)", retries)
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Errorf("calls = %d, want 1 (single attempt, no retry)", n)
	}
}

// TestRunWithFailRetry_CancelShortCircuits asserts context cancellation
// stops the retry loop even if budget remains.
func TestRunWithFailRetry_CancelShortCircuits(t *testing.T) {
	var calls int64
	stub := &stubExec{
		name: "stub",
		on: func(_ context.Context, _ int64, _ executor.Request) (executor.Response, error) {
			atomic.AddInt64(&calls, 1)
			return executor.Response{}, context.Canceled
		},
	}
	register(t, stub)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err, _ := runWithFailRetry(ctx, "stub", 100, executor.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := atomic.LoadInt64(&calls); n > 1 {
		t.Errorf("calls = %d, want <= 1 (must short-circuit on cancel)", n)
	}
}
