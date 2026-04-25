//go:build live

package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/executor"
)

// TestLiveGeminiOK is the smoke gate for the gemini executor against a
// real `gemini` binary on PATH. Gated by COUNCIL_LIVE_GEMINI=1 so plain
// `go test` (even with -tags live elsewhere) skips when the operator
// has not opted in.
//
// Asserts only that the prompt round-trips and gemini emits the literal
// "OK" — model-output assertions any tighter would flake on minor
// model-version drift.
func TestLiveGeminiOK(t *testing.T) {
	if os.Getenv("COUNCIL_LIVE_GEMINI") != "1" {
		t.Skip("COUNCIL_LIVE_GEMINI not set; skipping live gemini smoke")
	}

	g := &Gemini{}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	errf := filepath.Join(tmp, "err")

	_, err := g.Execute(context.Background(), executor.Request{
		Prompt:     "respond with the word OK and nothing else",
		Model:      "gemini-3.1-pro-preview",
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
