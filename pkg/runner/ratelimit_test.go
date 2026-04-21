package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanStderr(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantOK      bool
		wantRetryAt time.Duration
	}{
		{
			name:   "empty",
			body:   "",
			wantOK: false,
		},
		{
			name:   "non-rate-limit error",
			body:   "panic: runtime error\nexit status 1\n",
			wantOK: false,
		},
		{
			name:   "bare 429 alone is not enough",
			body:   "got status 429 from upstream\n",
			wantOK: false,
		},
		{
			name:        "rate_limit underscore form",
			body:        "Error: rate_limit exceeded — please retry\n",
			wantOK:      true,
			wantRetryAt: 0,
		},
		{
			name:        "rate limit space form",
			body:        "rate limit hit, please slow down\n",
			wantOK:      true,
			wantRetryAt: 0,
		},
		{
			name:        "429 with too many marker (forward order)",
			body:        "HTTP 429: too many requests\n",
			wantOK:      true,
			wantRetryAt: 0,
		},
		{
			name:        "429 with too many marker (reverse order)",
			body:        "too many requests (429)\n",
			wantOK:      true,
			wantRetryAt: 0,
		},
		{
			name:        "case insensitivity",
			body:        "RATE_LIMIT EXCEEDED\n",
			wantOK:      true,
			wantRetryAt: 0,
		},
		{
			name:        "with Retry-After hint",
			body:        "rate_limit exceeded\nRetry-After: 7s\n",
			wantOK:      true,
			wantRetryAt: 7 * time.Second,
		},
		{
			name:        "Retry-After without trailing s",
			body:        "rate_limit exceeded\nRetry-After: 12\n",
			wantOK:      true,
			wantRetryAt: 12 * time.Second,
		},
		{
			name:   "false positive guard: byte count contains 429",
			body:   "wrote 429 bytes to log\n",
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stderr")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, ok := scanStderr(path)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if got != c.wantRetryAt {
				t.Errorf("retryAfter = %s, want %s", got, c.wantRetryAt)
			}
		})
	}
}

func TestScanStderrMissingFile(t *testing.T) {
	// nonexistent path is not a rate-limit signal — return ok=false
	// rather than panicking. This matters because runOnce calls
	// scanStderr unconditionally on non-zero exit, even if the stderr
	// file failed to materialize.
	got, ok := scanStderr(filepath.Join(t.TempDir(), "does-not-exist"))
	if ok {
		t.Errorf("ok = true on missing file, want false")
	}
	if got != 0 {
		t.Errorf("retryAfter = %s on missing file, want 0", got)
	}
}

func TestScanStderrEmptyPath(t *testing.T) {
	got, ok := scanStderr("")
	if ok || got != 0 {
		t.Errorf("scanStderr(\"\") = (%s, %v), want (0, false)", got, ok)
	}
}
