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
	"github.com/fitz123/council/pkg/orchestrator"
	"github.com/fitz123/council/pkg/session"
)

// version is overridable via -ldflags "-X main.version=...". When unset,
// main() falls back to runtime/debug.ReadBuildInfo.
var version = ""

// Exit codes per docs/design/v1.md §4. Kept as named constants so test
// assertions read self-documenting rather than bare integers.
const (
	exitOK          = 0
	exitConfigError = 1
	exitQuorum      = 2
	exitJudge       = 3
	exitInterrupted = 130
)

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

	// v1 ships a single profile per location (default.yaml). Reject any
	// other profile name up-front so a caller passing -p foo does not
	// silently run against default.yaml. Multi-profile is a v2 hook.
	if profileName != "default" {
		fmt.Fprintf(stderr, "council: profile %q not supported in v1 (only \"default\" is available)\n", profileName)
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

	sess, err := createSession(cwd, profile, question)
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
	}

	switch {
	case err == nil:
		// Normal-mode stdout: the synthesis body, plus a trailing newline
		// if the body didn't already end with one. The newline is purely
		// terminal hygiene (so the next prompt doesn't land glued to the
		// last line of output). Callers that need byte-exact synthesis
		// should read rounds/1/judge/synthesis.md from the session folder.
		fmt.Fprint(stdout, v.Answer)
		if !strings.HasSuffix(v.Answer, "\n") {
			fmt.Fprintln(stdout)
		}
		return exitOK
	case errors.Is(err, orchestrator.ErrQuorumFailed):
		fmt.Fprintf(stderr, "council: quorum not met (see %s/verdict.json)\n", sess.Path)
		return exitQuorum
	case errors.Is(err, orchestrator.ErrJudgeFailed):
		fmt.Fprintf(stderr, "council: judge failed (see %s/verdict.json)\n", sess.Path)
		return exitJudge
	case errors.Is(err, orchestrator.ErrInterrupted):
		fmt.Fprintf(stderr, "council: interrupted (partial verdict at %s/verdict.json)\n", sess.Path)
		return exitInterrupted
	default:
		fmt.Fprintf(stderr, "council: %v\n", err)
		return exitConfigError
	}
}

// createSession allocates a session folder, retrying on os.ErrExist so a
// NewID petname collision or a leftover stale directory does not overwrite
// the earlier session's artifacts. The retry budget is tiny because
// collisions are ~10^-9 per second — three attempts is overwhelming.
func createSession(cwd string, profile *config.Profile, question string) (*session.Session, error) {
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		id := session.NewID(time.Now())
		sess, err := session.Create(cwd, id, profile, question)
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
  council --version
  council --help

Flags:
  -p, --profile NAME   Profile to use (default: "default"; v1 only accepts "default").
  -v, --verbose        Stream structured progress to stderr.
      --version        Print version and exit.

Exit codes:
  0    success
  1    config / validation error
  2    quorum not met
  3    judge failed
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
	fmt.Fprintf(w, "[%s] profile: %s (%d experts, quorum %d) from %s\n",
		ts, p.Name, len(p.Experts), p.Quorum, displaySource(source, sess))
	for _, e := range p.Experts {
		fmt.Fprintf(w, "[%s] spawning expert: %s (%s, %s)\n", ts, e.Name, e.Executor, e.Model)
	}
	fmt.Fprintf(w, "[%s] spawning judge (%s, %s)\n", ts, p.Judge.Executor, p.Judge.Model)
}

// logEnd emits the post-run timing summary in verbose mode. It reads
// timings from the verdict that orchestrator.Run already populated, so
// the on-disk verdict.json and the stderr stream agree byte-for-byte on
// per-role durations.
func logEnd(w io.Writer, sess *session.Session, v *session.VerdictV1) {
	if v == nil {
		return
	}
	ts := nowStamp()
	for _, e := range v.Rounds[0].Experts {
		fmt.Fprintf(w, "[%s] expert %s: %s in %.1fs (retries=%d)\n",
			ts, e.Name, e.Status, e.DurationSeconds, e.Retries)
	}
	if v.Rounds[0].Judge.Executor != "" {
		fmt.Fprintf(w, "[%s] judge: done in %.1fs (retries=%d)\n",
			ts, v.Rounds[0].Judge.DurationSeconds, v.Rounds[0].Judge.Retries)
	}
	fmt.Fprintf(w, "[%s] session %s: %.1fs total\n", ts, v.Status, v.DurationSeconds)
	fmt.Fprintf(w, "[%s] session folder: %s\n", ts, sess.Path)
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
