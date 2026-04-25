package main

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/fitz123/council/pkg/debate"
)

// stderrReporter implements debate.Reporter by rendering each stage event
// as a timestamped log line + (when applicable) a fenced artifact block.
// Multiple goroutines call OnStageDone concurrently — rounds fan out N
// experts in parallel — so a sync.Mutex guards the writer to keep output
// from interleaving mid-block.
type stderrReporter struct {
	mu sync.Mutex
	w  io.Writer
}

// newStderrReporter constructs the live verbose-stream renderer. Pass
// os.Stderr (or the test's stderr buffer) as w. The renderer is safe for
// concurrent use.
func newStderrReporter(w io.Writer) *stderrReporter {
	return &stderrReporter{w: w}
}

// OnStageDone fans out by event Kind. Each branch emits one timing line
// (matching the existing [HH:MM:SS] log style so the operator can grep
// across the run) followed by a `=== … ===` block for stages whose body
// is meaningful (success, carry-forward, voted ballots). Failed stages
// without a body emit just the timing line.
//
// All artifact bodies pass through stripControlBytes so a malformed or
// malicious LLM output cannot rewrite the operator's terminal state via
// ANSI/OSC escape sequences.
func (r *stderrReporter) OnStageDone(e debate.StageEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e.Kind {
	case "round-expert":
		r.renderRoundExpert(e)
	case "ballot":
		r.renderBallot(e)
	}
}

func (r *stderrReporter) renderRoundExpert(e debate.StageEvent) {
	ts := nowStamp()
	switch e.Participation {
	case "ok":
		if e.Resumed {
			fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) reused from cache\n",
				ts, e.Round, e.Label, e.RealName)
		} else {
			fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) ok in %.1fs (retries=%d)\n",
				ts, e.Round, e.Label, e.RealName, e.Duration.Seconds(), e.Retries)
		}
		fmt.Fprintf(r.w, "\n=== round %d expert %s (%s) ===\n", e.Round, e.Label, e.RealName)
		if len(e.Body) > 0 {
			fmt.Fprintln(r.w, stripControlBytes(strings.TrimRight(string(e.Body), "\n")))
		}
	case "carried":
		switch {
		case e.Resumed:
			fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) reused carried R1 body from cache\n",
				ts, e.Round, e.Label, e.RealName)
		case e.LimitErr != nil:
			fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) carried R1 body forward (R2 rate-limited: %s)\n",
				ts, e.Round, e.Label, e.RealName, e.LimitErr.Pattern)
		default:
			fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) carried R1 body forward (R2 failed)\n",
				ts, e.Round, e.Label, e.RealName)
		}
		fmt.Fprintf(r.w, "\n=== round %d expert %s (%s) — carried from R1 ===\n", e.Round, e.Label, e.RealName)
		if len(e.Body) > 0 {
			fmt.Fprintln(r.w, stripControlBytes(strings.TrimRight(string(e.Body), "\n")))
		}
	case "failed":
		if e.LimitErr != nil {
			fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) FAILED in %.1fs: rate-limited (%s)\n",
				ts, e.Round, e.Label, e.RealName, e.Duration.Seconds(), e.LimitErr.Pattern)
			return
		}
		fmt.Fprintf(r.w, "[%s] round %d expert %s (%s) FAILED in %.1fs\n",
			ts, e.Round, e.Label, e.RealName, e.Duration.Seconds())
	}
}

func (r *stderrReporter) renderBallot(e debate.StageEvent) {
	ts := nowStamp()
	switch {
	case e.VotedFor != "":
		if e.Resumed {
			fmt.Fprintf(r.w, "[%s] ballot %s (%s) reused from cache, voted for %s\n",
				ts, e.Label, e.RealName, e.VotedFor)
		} else {
			fmt.Fprintf(r.w, "[%s] ballot %s (%s) voted for %s in %.1fs\n",
				ts, e.Label, e.RealName, e.VotedFor, e.Duration.Seconds())
		}
	case e.LimitErr != nil:
		fmt.Fprintf(r.w, "[%s] ballot %s (%s) discarded (rate-limited) in %.1fs\n",
			ts, e.Label, e.RealName, e.Duration.Seconds())
	default:
		fmt.Fprintf(r.w, "[%s] ballot %s (%s) discarded (malformed) in %.1fs\n",
			ts, e.Label, e.RealName, e.Duration.Seconds())
	}
	fmt.Fprintf(r.w, "\n=== ballot %s (%s) ===\n", e.Label, e.RealName)
	if len(e.Body) > 0 {
		fmt.Fprintln(r.w, stripControlBytes(strings.TrimRight(string(e.Body), "\n")))
	}
	if e.VotedFor == "" {
		fmt.Fprintln(r.w, "(no vote — discarded)")
	}
}
