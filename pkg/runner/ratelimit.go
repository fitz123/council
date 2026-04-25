package runner

import (
	"fmt"
	"os"
	"strings"
)

// LimitError is the typed wrapper executors return when DetectLimit
// matches one of their per-vendor rate-limit markers in the captured
// stderr. The orchestrator (pkg/debate) classifies expert failures via
// errors.As(err, &limitErr); on quorum failure the per-CLI footer printed
// by cmd/council/main.go reads HelpCmd directly.
//
// Pattern is the marker that matched (lowercased substring as supplied by
// the executor). Tool is the executor's stable name (e.g. "claude-code",
// "codex", "gemini-cli"). HelpCmd is a one-liner the user can run to
// inspect their quota / billing.
type LimitError struct {
	Pattern string
	Tool    string
	HelpCmd string
}

// Error formats as a single line that includes Tool, the matched Pattern,
// and HelpCmd. Stable surface — the orchestrator logs this into structured
// output and the user-facing footer pulls HelpCmd out separately.
func (e *LimitError) Error() string {
	return fmt.Sprintf("rate limit hit on %s: matched %q — %s", e.Tool, e.Pattern, e.HelpCmd)
}

// DetectLimit reads the stderr file at path and reports the first pattern
// from patterns that appears as a (case-insensitive) substring of the
// file. Returns the matched pattern (verbatim, as supplied) and true on
// match; "" and false on no match, missing file, or empty path.
//
// Patterns are tried in slice order; the first match wins. Callers
// (executors) curate per-vendor lists. The runner does not own this
// list any more (ADR-0013) — the previous package-level rateLimitRE was
// claude-specific and broke when we added codex/gemini, whose surfaces
// don't share Anthropic's "rate_limit exceeded" string.
//
// We read the whole file into memory; rate-limit messages are tiny in
// practice (kilobytes at worst). If a future executor wraps a CLI such
// that stderr can grow unbounded on success too, we can move to a
// tail-only scan; for now executors only call DetectLimit on non-zero
// exit so the file is bounded by what the CLI produced before failing.
func DetectLimit(stderrPath string, patterns []string) (string, bool) {
	if stderrPath == "" || len(patterns) == 0 {
		return "", false
	}
	data, err := os.ReadFile(stderrPath)
	if err != nil {
		return "", false
	}
	body := strings.ToLower(string(data))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.Contains(body, strings.ToLower(p)) {
			return p, true
		}
	}
	return "", false
}
