package runner

import (
	"os"
	"regexp"
	"strconv"
	"time"
)

// Markers we treat as a 429 / rate-limit signal in stderr. The bare
// substring "429" was rejected during design (false matches on port
// numbers, byte counts, exit-code echoes, etc.). The accepted forms:
//
//   - "rate_limit" or "rate limit", optionally followed by " exceeded"
//   - "429" appearing AS A WORD next to a "too many" co-occurrence in
//     either order
//
// The regex is case-insensitive (?i) because upstream CLIs are
// inconsistent about capitalization.
var rateLimitRE = regexp.MustCompile(`(?i)(rate[_ ]limit(?: exceeded)?|\b429\b.*too many|too many.*\b429\b)`)

// retryAfterRE captures the seconds-value of a "Retry-After: N" header
// echoed into stderr. The trailing "s" is optional because some upstream
// formatters omit it.
var retryAfterRE = regexp.MustCompile(`Retry-After:\s*(\d+)s?`)

// scanStderr reads the captured stderr file and reports whether it
// looks like a rate-limit failure. When true, retryAfter carries any
// parsed Retry-After hint (or zero if none was found, in which case
// Run uses its 10-second default).
//
// We read the whole file into memory; rate-limit messages are tiny in
// practice (kilobytes at worst), and streaming would complicate the
// regex match across line boundaries. If a future executor wraps the
// CLI such that stderr can grow unbounded on success too, we should
// move to a tail-only scan; but for now design §10 only triggers the
// scan on non-zero exit, so the file is bounded by what the CLI
// produces before failing.
func scanStderr(path string) (retryAfter time.Duration, ok bool) {
	if path == "" {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	if !rateLimitRE.Match(data) {
		return 0, false
	}
	if m := retryAfterRE.FindSubmatch(data); m != nil {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n >= 0 {
			retryAfter = time.Duration(n) * time.Second
		}
	}
	return retryAfter, true
}
