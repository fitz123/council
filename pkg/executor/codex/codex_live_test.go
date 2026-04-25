//go:build live

package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
)

// TestLiveCodexOK is the smoke gate for the codex executor against a
// real `codex` binary on PATH. Gated by COUNCIL_LIVE_CODEX=1 so plain
// `go test` (even with -tags live elsewhere) skips when the operator
// has not opted in.
//
// Asserts only that the prompt round-trips and codex emits the literal
// "OK" — model-output assertions any tighter would flake on minor
// model-version drift.
func TestLiveCodexOK(t *testing.T) {
	if os.Getenv("COUNCIL_LIVE_CODEX") != "1" {
		t.Skip("COUNCIL_LIVE_CODEX not set; skipping live codex smoke")
	}

	c := &Codex{}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")

	_, err := c.Execute(context.Background(), executor.Request{
		Prompt:     "respond with the word OK and nothing else",
		Model:      "gpt-5.5",
		Timeout:    120 * time.Second,
		StdoutFile: out,
		StderrFile: errf,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body, _ := os.ReadFile(out)
	if !strings.Contains(strings.ToLower(string(body)), "ok") {
		t.Errorf("stdout missing 'OK' (case-insensitive): %q", string(body))
	}
}
