package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
)

// TestExecuteLive exercises the real `claude` CLI end-to-end. It is
// gated by COUNCIL_LIVE_CLAUDE=1 so CI (which has no Claude
// subscription) skips it cleanly. Local smoke runs that have
// authenticated `claude` set the env var and get real coverage of:
//
//   - the actual --model / --output-format / -p - flag surface
//   - stdin acceptance from a non-TTY subprocess
//   - non-empty stdout on a trivial prompt
//
// See the "Precondition" section of docs/plans/2026-04-20-v1-mvp.md for
// the manual flag-surface verification this test mirrors.
func TestExecuteLive(t *testing.T) {
	if os.Getenv("COUNCIL_LIVE_CLAUDE") != "1" {
		t.Skip("set COUNCIL_LIVE_CLAUDE=1 to run live claude integration test")
	}
	c := &ClaudeCode{} // real binary "claude" via PATH

	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, err := c.Execute(ctx, executor.Request{
		Prompt:     "say hi",
		Model:      "sonnet",
		Timeout:    60 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("live Execute: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("live ExitCode = %d, want 0", resp.ExitCode)
	}
	got, _ := os.ReadFile(out)
	if len(strings.TrimSpace(string(got))) == 0 {
		t.Errorf("live stdout was empty")
	}
}
