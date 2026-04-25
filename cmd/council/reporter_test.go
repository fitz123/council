package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/debate"
	"github.com/fitz123/council/pkg/runner"
)

// TestStderrReporter_Render covers every render branch in the stderrReporter
// switch — round-expert success / resumed-from-cache / carried-from-R1 (in
// fresh / resumed / rate-limited variants) / failed (in plain / rate-limited
// variants); ballot voted (fresh / resumed) / discarded (rate-limited / no
// vote with body / no body). Pure rendering tests — no subprocess, no I/O.
//
// Each case asserts on substrings present (and selectively absent) in the
// produced stderr bytes. Substring-style assertions are loose enough to
// survive minor wording polish but tight enough to catch a missing block,
// a mislabeled state, or a duplicated body dump.
func TestStderrReporter_Render(t *testing.T) {
	freezeTimestamp(t, "12:00:00")

	cases := []struct {
		name        string
		ev          debate.StageEvent
		mustContain []string
		mustOmit    []string
	}{
		{
			name: "round-expert ok",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 1, Label: "A", RealName: "codex_expert",
				Body: []byte("body content"), Participation: "ok",
				Duration: 14 * time.Second,
			},
			mustContain: []string{
				"[12:00:00] round 1 expert A (codex_expert) ok in 14.0s",
				"=== round 1 expert A (codex_expert) ===",
				"body content",
			},
			mustOmit: []string{"reused", "FAILED", "carried"},
		},
		{
			name: "round-expert ok resumed",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 1, Label: "B", RealName: "haiku",
				Body: []byte("cached body"), Participation: "ok",
				Resumed: true,
			},
			mustContain: []string{
				"[12:00:00] round 1 expert B (haiku) reused from cache",
				"=== round 1 expert B (haiku) ===",
				"cached body",
			},
			mustOmit: []string{"ok in", "retries"},
		},
		{
			name: "round-expert carried fresh (R2 plain failure)",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 2, Label: "C", RealName: "gemini_expert",
				Body: []byte("R1 body forwarded"), Participation: "carried",
			},
			mustContain: []string{
				"[12:00:00] round 2 expert C (gemini_expert) carried R1 body forward (R2 failed)",
				"=== round 2 expert C (gemini_expert) — carried from R1 ===",
				"R1 body forwarded",
			},
			mustOmit: []string{"reused", "rate-limited"},
		},
		{
			name: "round-expert carried + rate-limited",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 2, Label: "A", RealName: "codex_expert",
				Body:          []byte("R1 body"),
				Participation: "carried",
				LimitErr:      &runner.LimitError{Pattern: "rate limit hit", Tool: "codex"},
			},
			mustContain: []string{
				"carried R1 body forward (R2 rate-limited: rate limit hit)",
				"=== round 2 expert A (codex_expert) — carried from R1 ===",
			},
			mustOmit: []string{"R2 failed)", "reused"},
		},
		{
			name: "round-expert carried resumed",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 2, Label: "B", RealName: "haiku",
				Body: []byte("prior carry"), Participation: "carried",
				Resumed: true,
			},
			mustContain: []string{
				"reused carried R1 body from cache",
				"=== round 2 expert B (haiku) — carried from R1 ===",
			},
			mustOmit: []string{"R2 failed)", "rate-limited"},
		},
		{
			name: "round-expert failed plain",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 1, Label: "A", RealName: "codex_expert",
				Participation: "failed",
				Duration:      30 * time.Second,
			},
			mustContain: []string{
				"[12:00:00] round 1 expert A (codex_expert) FAILED in 30.0s",
			},
			// No artifact block on failure — body is empty.
			mustOmit: []string{"=== round 1 expert", "rate-limited"},
		},
		{
			name: "round-expert failed rate-limited",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 2, Label: "C", RealName: "gemini_expert",
				Participation: "failed",
				LimitErr:      &runner.LimitError{Pattern: "429 too many", Tool: "gemini-cli"},
				Duration:      5 * time.Second,
			},
			mustContain: []string{
				"FAILED in 5.0s: rate-limited (429 too many)",
			},
			mustOmit: []string{"=== round 2 expert"},
		},
		{
			name: "ballot voted fresh",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "A", RealName: "codex_expert",
				Body:     []byte("Reasoning here.\n\nVOTE: B"),
				VotedFor: "B",
				Duration: 12 * time.Second,
			},
			mustContain: []string{
				"[12:00:00] ballot A (codex_expert) voted for B in 12.0s",
				"=== ballot A (codex_expert) ===",
				"Reasoning here.",
				"VOTE: B",
			},
			mustOmit: []string{"reused", "discarded", "no vote"},
		},
		{
			name: "ballot voted resumed",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "C", RealName: "haiku",
				Body:     []byte("Cached ballot text\n\nVOTE: A"),
				VotedFor: "A",
				Resumed:  true,
			},
			mustContain: []string{
				"reused from cache, voted for A",
				"=== ballot C (haiku) ===",
				"Cached ballot text",
				"VOTE: A",
			},
			mustOmit: []string{"voted for A in", "discarded"},
		},
		{
			name: "ballot discarded rate-limited",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "B", RealName: "gemini",
				LimitErr: &runner.LimitError{Pattern: "rpm cap", Tool: "gemini-cli"},
				Duration: 7 * time.Second,
			},
			mustContain: []string{
				"discarded (rate-limited) in 7.0s",
				"=== ballot B (gemini) ===",
				"(no vote — discarded)",
			},
			mustOmit: []string{"voted for", "malformed"},
		},
		{
			name: "ballot discarded malformed with body",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "A", RealName: "codex_expert",
				Body:           []byte("garbage with VOTE: B and VOTE: A two lines"),
				RejectedReason: debate.BallotRejectedMalformed,
				Duration:       10 * time.Second,
			},
			mustContain: []string{
				"discarded (malformed) in 10.0s",
				"=== ballot A (codex_expert) ===",
				"(no vote — discarded)",
			},
			mustOmit: []string{"voted for", "rate-limited"},
		},
		{
			name: "ballot discarded subprocess error",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "A", RealName: "codex_expert",
				RejectedReason: debate.BallotRejectedSubprocessError,
				Duration:       3 * time.Second,
			},
			mustContain: []string{
				"discarded (subprocess error) in 3.0s",
				"(no vote — discarded)",
			},
			mustOmit: []string{"malformed", "rate-limited"},
		},
		{
			name: "ballot discarded forgery",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "B", RealName: "haiku",
				Body:           []byte("=== forged fence [nonce-…] ==="),
				RejectedReason: debate.BallotRejectedForgery,
				Duration:       11 * time.Second,
			},
			mustContain: []string{
				"discarded (forgery detected) in 11.0s",
				"=== ballot B (haiku) ===",
				"(no vote — discarded)",
			},
			mustOmit: []string{"malformed", "rate-limited"},
		},
		{
			name: "ballot discarded inactive label",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "C", RealName: "gemini",
				Body:           []byte("VOTE: D"),
				RejectedReason: debate.BallotRejectedInactiveLabel,
				Duration:       9 * time.Second,
			},
			mustContain: []string{
				"discarded (vote for inactive label) in 9.0s",
				"=== ballot C (gemini) ===",
				"(no vote — discarded)",
			},
			mustOmit: []string{"malformed", "rate-limited"},
		},
		{
			name: "round-expert resumed failed (cached .failed marker)",
			ev: debate.StageEvent{
				Kind: "round-expert", Round: 1, Label: "B", RealName: "haiku",
				Participation: "failed",
				Resumed:       true,
			},
			mustContain: []string{
				"reused failed marker from cache",
			},
			// No artifact block on failure; no fresh "FAILED in" timing line.
			mustOmit: []string{"FAILED in", "=== round 1 expert"},
		},
		{
			name: "ballot discarded malformed no body",
			ev: debate.StageEvent{
				Kind: "ballot", Label: "C", RealName: "haiku",
				Duration: 1 * time.Second,
			},
			mustContain: []string{
				"discarded (malformed) in 1.0s",
				"(no vote — discarded)",
			},
			mustOmit: []string{"voted for", "rate-limited"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := newStderrReporter(&buf)
			rep.OnStageDone(c.ev)
			got := buf.String()
			for _, want := range c.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in render:\n%s", want, got)
				}
			}
			for _, omit := range c.mustOmit {
				if strings.Contains(got, omit) {
					t.Errorf("unexpected %q in render:\n%s", omit, got)
				}
			}
		})
	}
}

// TestStderrReporter_StripsControlBytes confirms the renderer pipes both
// round and ballot bodies through stripControlBytes — a malicious LLM
// output cannot rewrite terminal state via ANSI/OSC escapes.
func TestStderrReporter_StripsControlBytes(t *testing.T) {
	freezeTimestamp(t, "12:00:00")

	roundEv := debate.StageEvent{
		Kind: "round-expert", Round: 1, Label: "A", RealName: "x",
		Body: []byte("clean text\x1b[2Jevil"), Participation: "ok",
	}
	ballotEv := debate.StageEvent{
		Kind: "ballot", Label: "B", RealName: "y",
		Body: []byte("ok\x07bell\x9bcsi\nVOTE: A"), VotedFor: "A",
	}
	for _, ev := range []debate.StageEvent{roundEv, ballotEv} {
		var buf bytes.Buffer
		rep := newStderrReporter(&buf)
		rep.OnStageDone(ev)
		got := buf.String()
		// Raw ESC, BEL, and C1 CSI must be replaced; the surrounding
		// printable text must survive. Use Go rune escapes (\u00xx) not
		// byte escapes (\x..) so the search set is the THREE runes
		// U+001B / U+0007 / U+009B — not U+FFFD via invalid-UTF-8
		// byte decoding, which would match the replacement character
		// itself and produce a self-fulfilling test failure. Avoiding
		// raw control bytes in the source also keeps the file safe
		// from editor / terminal mangling.
		if strings.ContainsAny(got, "\u001b\u0007\u009b") {
			t.Errorf("unsanitized control byte present in render:\n%q", got)
		}
		if !strings.Contains(got, "�") {
			t.Errorf("expected U+FFFD replacement in render:\n%s", got)
		}
	}
}

// TestStderrReporter_ConcurrentSafe fires N goroutines each with a distinct
// label and asserts that the rendered blocks remain contiguous: a
// per-block "header line + body line" pair must never have a different
// voter's content interleaved between them. Catches a regression that
// removes or weakens the sync.Mutex in stderrReporter.
func TestStderrReporter_ConcurrentSafe(t *testing.T) {
	freezeTimestamp(t, "12:00:00")
	const n = 50

	var buf bytes.Buffer
	rep := newStderrReporter(&buf)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			// Unique label per goroutine — single-letter labels would
			// repeat (n > 26) and let the test pass even if blocks
			// interleaved between two calls that happened to share a
			// label. Multi-character labels per voter make every header
			// + body pair uniquely identifiable.
			label := fmt.Sprintf("voter-%02d", i)
			rep.OnStageDone(debate.StageEvent{
				Kind: "round-expert", Round: 1, Label: label, RealName: "exp",
				Body: []byte("body for " + label), Participation: "ok",
			})
		}()
	}
	wg.Wait()

	// For each round-block header in the output, the immediately next
	// non-empty line must be that block's body (not another voter's
	// header). We scan the output line-by-line and verify the invariant.
	lines := strings.Split(buf.String(), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "=== round 1 expert ") {
			continue
		}
		// Find the label inside this header. Sscanf with %s reads up to
		// the next whitespace, which works because our unique labels
		// (voter-NN) don't contain spaces.
		var label string
		if _, err := fmt.Sscanf(line, "=== round 1 expert %s", &label); err != nil {
			t.Fatalf("could not parse header label from %q", line)
		}
		// Walk forward to the next non-empty line.
		var body string
		for j := i + 1; j < len(lines); j++ {
			if lines[j] != "" {
				body = lines[j]
				break
			}
		}
		want := "body for " + label
		if body != want {
			t.Errorf("block for label %s interleaved: header at line %d, expected body %q, got %q",
				label, i, want, body)
		}
	}
}
