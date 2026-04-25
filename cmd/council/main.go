// Package main is the council binary entry point. It parses CLI flags,
// resolves the profile, materializes the session folder, runs the
// orchestrator, and maps its sentinel errors to the exit codes documented
// in docs/design/v1.md §4.
//
// The entry point is split into main() and run() so tests can drive run()
// with a fake stdin/stdout/stderr and inspect the returned exit code
// without invoking os.Exit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	// Executor registration is split across executors_release.go (default
	// build) and executors_testbinary.go (gated by `-tags testbinary`),
	// so the smoke test binary can substitute in mock executors without
	// touching the production binary's wiring.

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/debate"
	"github.com/fitz123/council/pkg/orchestrator"
	"github.com/fitz123/council/pkg/session"
)

// version is overridable via -ldflags "-X main.version=...". When unset,
// main() falls back to runtime/debug.ReadBuildInfo.
var version = ""

// Exit codes per docs/design/v2.md §4. Kept as named constants so test
// assertions read self-documenting rather than bare integers. v2 drops the
// judge role, so exit 3 is retired; the tie path ("no_consensus") folds
// into exit 2 alongside quorum-failed runs. v3 adds exit 6 for the
// rate-limit-induced quorum failure (ADR-0013) so a vendor outage is
// distinguishable from a real disagreement at the shell level.
const (
	exitOK                  = 0
	exitConfigError         = 1
	exitQuorum              = 2
	exitRateLimitQuorumFail = 6
	exitInterrupted         = 130
)

// resumeSubcommand is the literal first-positional that switches `council`
// from "run a fresh session" to "continue an existing one" (D14). Kept as a
// constant so tests and the help text stay in sync.
const resumeSubcommand = "resume"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable body of main. It returns the intended exit code
// rather than calling os.Exit directly so tests can exercise every path
// without forking a subprocess.
//
// Signal handling: the root context is constructed in main() via
// signal.NotifyContext and passed in here. Cancellation propagates to
// pkg/runner (which kills the process group) and to the orchestrator
// (which flushes an "interrupted" verdict before returning). The
// orchestrator's guarantee that verdict.json exists on disk BEFORE it
// returns ErrInterrupted is what lets us exit 130 immediately without a
// second flush step here.
func run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Subcommand dispatch. Detected before flag parsing so each
	// subcommand's flag set can carry its own options without colliding
	// with the main flag set's -p / -v / --version.
	if len(argv) > 0 && argv[0] == initSubcommand {
		return runInit(argv[1:], stdout, stderr)
	}
	if len(argv) > 0 && argv[0] == resumeSubcommand {
		return runResume(ctx, argv[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("council", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printHelp(stderr) }

	var (
		profileName string
		verbose     bool
		showVersion bool
	)
	fs.StringVar(&profileName, "profile", "default", "Profile to use.")
	fs.StringVar(&profileName, "p", "default", "Profile to use (shorthand).")
	fs.BoolVar(&verbose, "verbose", false, "Stream structured progress to stderr.")
	fs.BoolVar(&verbose, "v", false, "Stream structured progress to stderr (shorthand).")
	fs.BoolVar(&showVersion, "version", false, "Print version and exit.")

	if err := fs.Parse(argv); err != nil {
		// flag.ContinueOnError already wrote the error + usage to stderr.
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitConfigError
	}

	if showVersion {
		fmt.Fprintf(stdout, "council %s\n", resolveVersion())
		return exitOK
	}

	// Positional argument = the question. A single dash ("-") means read
	// stdin until EOF so callers can pipe prompts that exceed argv limits.
	positional := fs.Args()
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "council: expected exactly one question argument (use '-' to read from stdin)")
		printHelp(stderr)
		return exitConfigError
	}
	question, err := readQuestion(positional[0], stdin)
	if err != nil {
		fmt.Fprintf(stderr, "council: read question: %v\n", err)
		return exitConfigError
	}
	if strings.TrimSpace(question) == "" {
		fmt.Fprintln(stderr, "council: question is empty")
		return exitConfigError
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "council: getcwd: %v\n", err)
		return exitConfigError
	}

	// v2 ships a single profile per location (default.yaml). Reject any
	// other profile name up-front so a caller passing -p foo does not
	// silently run against default.yaml. Multi-profile is a v3 hook.
	if profileName != "default" {
		fmt.Fprintf(stderr, "council: profile %q not supported (only \"default\" is available)\n", profileName)
		return exitConfigError
	}

	profile, source, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "council: load config: %v\n", err)
		return exitConfigError
	}

	// Fail fast on executor typos. Without this check, a bad executor name
	// leaks through as a runtime "failed" expert (could even pass quorum)
	// or as exit 3 for the judge; both should be exit 1.
	if err := orchestrator.Validate(profile); err != nil {
		fmt.Fprintf(stderr, "council: %v\n", err)
		return exitConfigError
	}

	// Preflight: every expert's BinaryName resolves on PATH. Without
	// this, a missing CLI surfaces as N near-identical "exec: ... not
	// found" lines deep inside debate.Run instead of one up-front
	// failure naming the binary and the offending profile line.
	if err := Preflight(profile); err != nil {
		fmt.Fprintf(stderr, "council: %v\n", err)
		return exitConfigError
	}

	nonce, err := debate.GenerateNonce()
	if err != nil {
		fmt.Fprintf(stderr, "council: generate session nonce: %v\n", err)
		return exitConfigError
	}

	sess, err := createSession(cwd, profile, nonce, question)
	if err != nil {
		fmt.Fprintf(stderr, "council: create session: %v\n", err)
		return exitConfigError
	}

	if verbose {
		logStart(stderr, sess, profile, source)
	}

	v, err := orchestrator.Run(ctx, profile, question, sess)

	if verbose {
		logEnd(stderr, sess, v)
		logArtifacts(stderr, sess, v)
	}

	switch {
	case err == nil:
		// Normal-mode stdout: the winner's R2 body, plus a trailing newline
		// if the body didn't already end with one. The newline is purely
		// terminal hygiene (so the next prompt doesn't land glued to the
		// last line of output). Callers that need byte-exact output should
		// read output.md from the session folder.
		fmt.Fprint(stdout, v.Answer)
		if !strings.HasSuffix(v.Answer, "\n") {
			fmt.Fprintln(stdout)
		}
		return exitOK
	case errors.Is(err, orchestrator.ErrInjectionInQuestion):
		fmt.Fprintf(stderr, "council: injection suspected in question (see %s/verdict.json)\n", sess.Path)
		return exitConfigError
	case errors.Is(err, debate.ErrRateLimitQuorumFail):
		fmt.Fprintf(stderr, "council: quorum not met due to rate limits (see %s/verdict.json)\n", sess.Path)
		writeRateLimitFooter(stderr, v)
		return exitRateLimitQuorumFail
	case errors.Is(err, debate.ErrQuorumFailedR1):
		fmt.Fprintf(stderr, "council: round 1 quorum not met (see %s/verdict.json)\n", sess.Path)
		return exitQuorum
	case errors.Is(err, debate.ErrQuorumFailedR2):
		fmt.Fprintf(stderr, "council: round 2 quorum not met (see %s/verdict.json)\n", sess.Path)
		return exitQuorum
	case errors.Is(err, orchestrator.ErrNoConsensus):
		fmt.Fprintf(stderr, "council: no consensus — tied candidates surfaced in %s/output-*.md\n", sess.Path)
		return exitQuorum
	case errors.Is(err, orchestrator.ErrInterrupted):
		fmt.Fprintf(stderr, "council: interrupted (partial verdict at %s/verdict.json)\n", sess.Path)
		return exitInterrupted
	default:
		fmt.Fprintf(stderr, "council: %v\n", err)
		return exitConfigError
	}
}

// runResume implements `council resume [--session <id>]` (D14). It locates
// (or accepts an explicit) incomplete session, restores its state, and
// reruns orchestrator.Run against it — the round runners and ballot runner
// are idempotent on their per-stage .done markers, so completed work is
// skipped and only the missing stages re-execute.
//
// Exit codes mirror the fresh-run path with one addition: ErrNoResumableSession
// maps to exit 1 ("no_resumable_session") so an operator who runs `council
// resume` against a clean tree gets a clear signal rather than a generic
// "config error" miss.
func runResume(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("council resume", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var sessionID string
	fs.StringVar(&sessionID, "session", "", "Resume the named session ID (default: newest incomplete).")

	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitConfigError
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "council resume: unexpected positional argument; use --session <id>")
		return exitConfigError
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "council resume: getcwd: %v\n", err)
		return exitConfigError
	}
	sessionsRoot := filepath.Join(cwd, ".council", "sessions")

	var sessionPath string
	if sessionID != "" {
		// Reject any path-traversal or separator tokens in the session
		// ID before joining: filepath.Join("..","..","x") escapes
		// sessionsRoot and lets an attacker point at a directory
		// whose profile.snapshot.yaml they control. NewID produces a
		// single pathname segment (timestamp + petname), so a legal ID
		// never contains these.
		if sessionID == "." || sessionID == ".." ||
			strings.ContainsAny(sessionID, `/\`) ||
			strings.Contains(sessionID, "..") {
			fmt.Fprintf(stderr, "council resume: invalid session id %q\n", sessionID)
			return exitConfigError
		}
		sessionPath = filepath.Join(sessionsRoot, sessionID)
		if _, err := os.Stat(sessionPath); err != nil {
			fmt.Fprintf(stderr, "council resume: session %q: %v\n", sessionID, err)
			return exitConfigError
		}
		// Explicit session IDs still have to satisfy the D14 finality
		// predicate — otherwise `council resume --session <final-id>`
		// would mutate a terminal session's verdict/output (rewrite
		// timestamps, possibly change the winner if ballots got re-run).
		if !session.IsResumable(sessionPath) {
			fmt.Fprintf(stderr, "council resume: session %q is not resumable (already final or never progressed)\n", sessionID)
			return exitConfigError
		}
	} else {
		sessionPath, err = session.FindIncomplete(sessionsRoot)
		if err != nil {
			if errors.Is(err, session.ErrNoResumableSession) {
				fmt.Fprintln(stderr, "council resume: no resumable session found")
				return exitConfigError
			}
			fmt.Fprintf(stderr, "council resume: find incomplete: %v\n", err)
			return exitConfigError
		}
	}

	sess, err := session.LoadExisting(sessionPath)
	if err != nil {
		fmt.Fprintf(stderr, "council resume: load %s: %v\n", sessionPath, err)
		return exitConfigError
	}
	profile, _, err := config.LoadSnapshot(filepath.Join(sess.Path, "profile.snapshot.yaml"))
	if err != nil {
		fmt.Fprintf(stderr, "council resume: load profile snapshot: %v\n", err)
		return exitConfigError
	}
	question, err := session.LoadQuestion(sess.Path)
	if err != nil {
		fmt.Fprintf(stderr, "council resume: load question: %v\n", err)
		return exitConfigError
	}

	// Clean up a stranded verdict.json.tmp from a prior crash so the atomic
	// O_EXCL writer in pkg/session.WriteVerdict can recreate it. Renamed
	// .tmp files are gone after a successful WriteVerdict; one left over
	// implies a crash between create and rename.
	_ = os.Remove(filepath.Join(sess.Path, "verdict.json.tmp"))

	if err := orchestrator.Validate(profile); err != nil {
		fmt.Fprintf(stderr, "council resume: %v\n", err)
		return exitConfigError
	}
	if err := Preflight(profile); err != nil {
		fmt.Fprintf(stderr, "council resume: %v\n", err)
		return exitConfigError
	}

	v, err := orchestrator.Run(ctx, profile, question, sess)
	switch {
	case err == nil:
		fmt.Fprint(stdout, v.Answer)
		if !strings.HasSuffix(v.Answer, "\n") {
			fmt.Fprintln(stdout)
		}
		return exitOK
	case errors.Is(err, orchestrator.ErrInjectionInQuestion):
		fmt.Fprintf(stderr, "council resume: injection suspected in question (see %s/verdict.json)\n", sess.Path)
		return exitConfigError
	case errors.Is(err, debate.ErrRateLimitQuorumFail):
		fmt.Fprintf(stderr, "council resume: quorum not met due to rate limits (see %s/verdict.json)\n", sess.Path)
		writeRateLimitFooter(stderr, v)
		return exitRateLimitQuorumFail
	case errors.Is(err, debate.ErrQuorumFailedR1):
		fmt.Fprintf(stderr, "council resume: round 1 quorum not met (see %s/verdict.json)\n", sess.Path)
		return exitQuorum
	case errors.Is(err, debate.ErrQuorumFailedR2):
		fmt.Fprintf(stderr, "council resume: round 2 quorum not met (see %s/verdict.json)\n", sess.Path)
		return exitQuorum
	case errors.Is(err, orchestrator.ErrNoConsensus):
		fmt.Fprintf(stderr, "council resume: no consensus — tied candidates surfaced in %s/output-*.md\n", sess.Path)
		return exitQuorum
	case errors.Is(err, orchestrator.ErrInterrupted):
		fmt.Fprintf(stderr, "council resume: interrupted (partial verdict at %s/verdict.json)\n", sess.Path)
		return exitInterrupted
	default:
		fmt.Fprintf(stderr, "council resume: %v\n", err)
		return exitConfigError
	}
}

// createSession allocates a session folder, retrying on os.ErrExist so a
// NewID petname collision or a leftover stale directory does not overwrite
// the earlier session's artifacts. The retry budget is tiny because
// collisions are ~10^-9 per second — three attempts is overwhelming.
//
// nonce is the debate-engine session nonce (pkg/debate.GenerateNonce); it is
// persisted in profile.snapshot.yaml so `council resume` can recover it.
func createSession(cwd string, profile *config.Profile, nonce, question string) (*session.Session, error) {
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		id := session.NewID(time.Now())
		sess, err := session.Create(cwd, id, profile, nonce, question)
		if err == nil {
			return sess, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("session id collided %d times: %w", maxAttempts, lastErr)
}

// readQuestion returns the question text for the positional argument. A
// bare "-" reads stdin to EOF; anything else is returned verbatim.
func readQuestion(arg string, stdin io.Reader) (string, error) {
	if arg == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return arg, nil
}

// resolveVersion picks the best version string available: an explicit
// -ldflags injection wins; otherwise we fall back to the module version
// baked in by `go install`. Unknown sources degrade to "dev".
func resolveVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// printHelp writes the flag summary. Kept separate from fs.PrintDefaults
// so the two-line synopsis + exit-code table appear together and match
// the surface documented in docs/design/v1.md §4.
func printHelp(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  council [flags] "question"
  council [flags] -          # read question from stdin (until EOF)
  council init [--force]
  council resume [--session ID]
  council --version
  council --help

Flags:
  -p, --profile NAME   Profile to use (default: "default"; only "default" is currently supported).
  -v, --verbose        Stream structured progress to stderr.
      --version        Print version and exit.

Subcommands:
  init                 Probe installed CLIs and write ~/.config/council/default.yaml
                       with one expert per verified CLI. Idempotent without --force.
  resume               Continue the newest incomplete session, or the one named
                       by --session. Re-runs any stage missing its .done marker.

Exit codes:
  0    success
  1    config / validation / injection error / no resumable session
  2    quorum not met, or no consensus (tied ballots)
  6    quorum not met due to vendor rate-limits (per-CLI footer printed)
  130  interrupted (SIGINT/SIGTERM)`)
}

// logStart writes the pre-run summary in verbose mode. Matches the format
// in docs/design/v1.md §11 — timestamps, profile summary, one "spawning
// expert:" line per expert, one "spawning judge" line.
//
// We don't try to emit per-expert "done" lines here because the
// orchestrator's Run is a single blocking call from our perspective; the
// per-stage timings are on disk in verdict.json.rounds[0].experts[].
// logEnd pulls them back out so the user sees them in chronological order.
func logStart(w io.Writer, sess *session.Session, p *config.Profile, source string) {
	ts := nowStamp()
	fmt.Fprintf(w, "[%s] council %s — session %s\n", ts, resolveVersion(), sess.ID)
	fmt.Fprintf(w, "[%s] profile: %s (%d experts, quorum %d, rounds %d) from %s\n",
		ts, p.Name, len(p.Experts), p.Quorum, p.Rounds, displaySource(source, sess))
	for _, e := range p.Experts {
		fmt.Fprintf(w, "[%s] spawning expert: %s (%s, %s)\n", ts, e.Name, e.Executor, e.Model)
	}
}

// logEnd emits the post-run timing summary in verbose mode. It reads
// timings from the verdict that orchestrator.Run already populated, so
// the on-disk verdict.json and the stderr stream agree byte-for-byte on
// per-role durations.
func logEnd(w io.Writer, sess *session.Session, v *session.Verdict) {
	if v == nil {
		return
	}
	ts := nowStamp()
	for idx, r := range v.Rounds {
		for _, e := range r.Experts {
			fmt.Fprintf(w, "[%s] round %d expert %s (%s): %s in %.1fs (retries=%d)\n",
				ts, idx+1, e.Label, e.RealName, e.Participation, e.DurationSeconds, e.Retries)
		}
	}
	if v.Voting != nil {
		if v.Voting.Winner != "" {
			fmt.Fprintf(w, "[%s] voting: winner %s\n", ts, v.Voting.Winner)
		} else if len(v.Voting.TiedCandidates) > 0 {
			fmt.Fprintf(w, "[%s] voting: tied candidates %v\n", ts, v.Voting.TiedCandidates)
		}
	}
	fmt.Fprintf(w, "[%s] session %s: %.1fs total\n", ts, v.Status, v.DurationSeconds)
	fmt.Fprintf(w, "[%s] session folder: %s\n", ts, sess.Path)
}

// logArtifacts dumps each expert's round output and per-voter ballot blocks
// to w after the timing summary. Lets a verbose run answer "who said what
// and who voted for whom" without having to open files in the session folder.
// Each section is independent: rounds with no readable output.md are skipped,
// individual unreadable per-expert output.md files are skipped, and the
// ballot section emits whenever v.Voting.Ballots is populated even if no
// rounds completed (e.g. resumed session that only re-ran the voting stage).
// All artifact bodies are scrubbed of C0/DEL control bytes via
// stripControlBytes before being written, so a malicious or malformed LLM
// output cannot rewrite the operator's terminal state via ANSI/OSC escapes.
func logArtifacts(w io.Writer, sess *session.Session, v *session.Verdict) {
	if v == nil || sess == nil {
		return
	}
	for idx, r := range v.Rounds {
		for _, e := range r.Experts {
			path := filepath.Join(sess.RoundExpertDir(idx+1, e.Label), "output.md")
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "\n=== round %d expert %s (%s) ===\n", idx+1, e.Label, e.RealName)
			fmt.Fprintln(w, stripControlBytes(strings.TrimRight(string(body), "\n")))
		}
	}
	if v.Voting != nil && len(v.Voting.Ballots) > 0 {
		realName := make(map[string]string, len(v.Experts))
		for _, e := range v.Experts {
			realName[e.Label] = e.RealName
		}
		for _, b := range v.Voting.Ballots {
			fmt.Fprintf(w, "\n=== ballot %s (%s) ===\n", b.VoterLabel, realName[b.VoterLabel])
			path := filepath.Join(sess.Path, "voting", "votes", b.VoterLabel+".txt")
			body, err := os.ReadFile(path)
			switch {
			case err == nil:
				fmt.Fprintln(w, stripControlBytes(strings.TrimRight(string(body), "\n")))
			case b.VotedFor != "":
				// File missing/unreadable but the verdict has a vote
				// recorded — fall back to the structured outcome so the
				// operator can always see "who voted for whom" even if
				// the on-disk artifact was wiped between the run and
				// this dump.
				fmt.Fprintf(w, "VOTE: %s\n", b.VotedFor)
			}
			if b.VotedFor == "" {
				fmt.Fprintln(w, "(no vote — discarded)")
			}
		}
	}
}

// stripControlBytes scrubs LLM-controlled artifact bodies of terminal-
// interpreting control characters before they are written to the operator's
// verbose stderr stream. Tab, newline, and carriage return are preserved so
// multi-line and tabbed content renders normally; every other byte in the
// C0 range (U+0000..U+001F), DEL (U+007F), and the C1 range (U+0080..U+009F,
// which includes 0x9B = CSI on 8-bit-clean terminals) becomes U+FFFD so a
// malformed or malicious peer output cannot clear the screen, rewrite
// terminal state, or spoof subsequent council log lines.
func stripControlBytes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// displaySource renders the config source for the verbose preamble. The
// embedded source is shown literally ("embedded"); file sources are shown
// relative to the session's cwd when possible, otherwise absolute.
func displaySource(source string, sess *session.Session) string {
	if source == config.SourceEmbedded {
		return source
	}
	cwd := filepath.Dir(filepath.Dir(filepath.Dir(sess.Path))) // sess.Path = <cwd>/.council/sessions/<id>
	if rel, err := filepath.Rel(cwd, source); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return source
}

// nowStamp returns an HH:MM:SS stamp in UTC, matching §11's sample log.
// Indirected via a package var so tests can freeze the clock without
// resorting to stdlib monkey-patching.
var nowStamp = func() string { return time.Now().UTC().Format("15:04:05") }

// writeRateLimitFooter prints one HelpCmd hint per UNIQUE executor in
// v.RateLimits to w. The footer is appended to the rate-limit-quorum-fail
// stderr branch (exit 6) so the operator gets a vendor-specific next-step
// without having to open verdict.json. Order matches first-occurrence in the
// slice; deduplication is by Executor field. v may be nil if the orchestrator
// returned an error before writing any rounds — the function is a no-op in
// that case so the surrounding case branch stays straight-line.
func writeRateLimitFooter(w io.Writer, v *session.Verdict) {
	if v == nil {
		return
	}
	seen := make(map[string]bool, len(v.RateLimits))
	for _, e := range v.RateLimits {
		if seen[e.Executor] {
			continue
		}
		seen[e.Executor] = true
		fmt.Fprintf(w, "  %s: %s\n", e.Executor, e.HelpCmd)
	}
}
