package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectLimit covers the public DetectLimit helper that replaced the
// pre-ADR-0013 scanStderr internals. Executors own their per-vendor marker
// list and pass it in; the runner is no longer in the rate-limit-pattern
// business beyond "case-insensitive first-match substring scan of a file."
func TestDetectLimit(t *testing.T) {
	patterns := []string{
		"you've hit your limit",
		"usage limit exceeded",
		"anthropic rate limit",
	}

	cases := []struct {
		name        string
		body        string
		patterns    []string
		wantPattern string
		wantOK      bool
	}{
		{
			name:     "empty body no match",
			body:     "",
			patterns: patterns,
			wantOK:   false,
		},
		{
			name:     "non-rate-limit error",
			body:     "panic: runtime error\nexit status 1\n",
			patterns: patterns,
			wantOK:   false,
		},
		{
			name:        "exact match",
			body:        "Error: usage limit exceeded — please retry\n",
			patterns:    patterns,
			wantPattern: "usage limit exceeded",
			wantOK:      true,
		},
		{
			name:        "case insensitive match",
			body:        "ERROR: ANTHROPIC RATE LIMIT reached\n",
			patterns:    patterns,
			wantPattern: "anthropic rate limit",
			wantOK:      true,
		},
		{
			name:        "first match wins (earlier pattern in patterns slice)",
			body:        "you've hit your limit. usage limit exceeded.\n",
			patterns:    patterns,
			wantPattern: "you've hit your limit",
			wantOK:      true,
		},
		{
			name:        "first match wins regardless of position in body",
			body:        "usage limit exceeded. you've hit your limit.\n",
			patterns:    []string{"you've hit your limit", "usage limit exceeded"},
			wantPattern: "you've hit your limit",
			wantOK:      true,
		},
		{
			name:     "empty patterns slice never matches",
			body:     "anything goes here, even rate limit\n",
			patterns: nil,
			wantOK:   false,
		},
		{
			name:        "single pattern",
			body:        "RESOURCE_EXHAUSTED: quota gone\n",
			patterns:    []string{"RESOURCE_EXHAUSTED"},
			wantPattern: "RESOURCE_EXHAUSTED",
			wantOK:      true,
		},
		{
			name:        "case insensitivity covers pattern too",
			body:        "resource_exhausted\n",
			patterns:    []string{"RESOURCE_EXHAUSTED"},
			wantPattern: "RESOURCE_EXHAUSTED",
			wantOK:      true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stderr")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, ok := DetectLimit(path, c.patterns)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if got != c.wantPattern {
				t.Errorf("pattern = %q, want %q", got, c.wantPattern)
			}
		})
	}
}

func TestDetectLimitMissingFile(t *testing.T) {
	// nonexistent path is not a rate-limit signal — return ok=false rather
	// than panicking. Callers can call DetectLimit unconditionally on
	// non-zero exit even when the stderr file failed to materialize.
	got, ok := DetectLimit(filepath.Join(t.TempDir(), "does-not-exist"), []string{"rate limit"})
	if ok {
		t.Errorf("ok = true on missing file, want false")
	}
	if got != "" {
		t.Errorf("pattern = %q on missing file, want empty", got)
	}
}

func TestDetectLimitEmptyPath(t *testing.T) {
	got, ok := DetectLimit("", []string{"rate limit"})
	if ok || got != "" {
		t.Errorf(`DetectLimit("", patterns) = (%q, %v), want ("", false)`, got, ok)
	}
}

func TestLimitErrorFormatsConsistently(t *testing.T) {
	// LimitError.Error formats as: rate limit hit on <tool>: matched "<pattern>" — <helpcmd>
	// Stable surface — the per-CLI footer printed by cmd/council/main.go on
	// ExitRateLimitQuorumFail (=6) reads HelpCmd directly, but the orchestrator
	// also logs Error() into structured output for diagnostics.
	le := &LimitError{Pattern: "usage limit exceeded", Tool: "claude-code", HelpCmd: "claude /usage"}
	got := le.Error()
	wantSubs := []string{
		"rate limit hit on claude-code",
		`matched "usage limit exceeded"`,
		"claude /usage",
	}
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Errorf("LimitError.Error() = %q, want substring %q", got, s)
		}
	}
}
