package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fitz123/council/defaults"
	"github.com/fitz123/council/pkg/executor"
)

// initSubcommand is the literal first-positional that switches `council`
// from "run a question" to "materialise a default profile". Detected
// before flag parsing so init's flag set can carry --force without
// colliding with the main set.
const initSubcommand = "init"

// homeDir is var-indirect so tests can override the user home directory
// without manipulating the environment. Production wiring is
// os.UserHomeDir; init_test.go points it at t.TempDir() to keep init
// off the real $HOME.
var homeDir = os.UserHomeDir

// initLookPath is the os/exec.LookPath indirection used by init's CLI
// detection step. Separate var from preflight.go's lookPath so the two
// can be stubbed independently in tests.
var initLookPath = exec.LookPath

// probeFunc is the seam tests use to stub the live-CLI probe. Production
// wiring (liveProbe) actually spawns the binary via Executor.Execute;
// tests inject a deterministic stub returning canned (ok, reason).
type probeFunc func(ctx context.Context, ex executor.Executor, model string) (ok bool, reason string)

// probe is the package-level probe seam. Tests reassign and restore via
// t.Cleanup. The default implementation runs a real "respond with OK"
// probe against the executor's CLI.
var probe probeFunc = liveProbe

// candidate is one row in init's CLI detection table. The order here is
// the order experts appear in the generated profile YAML; matches the
// sample in docs/plans/2026-04-25-v3-multi-cli.md §"Generated profile
// shape".
type candidate struct {
	expertName string
	executor   string
	model      string
}

// initCandidates are the CLIs init knows how to probe. New CLIs are
// added here in one place; everything downstream (detection, YAML
// emission, summary lines) iterates this list.
var initCandidates = []candidate{
	{expertName: "claude_expert", executor: "claude-code", model: "opus"},
	{expertName: "codex_expert", executor: "codex", model: "gpt-5.5"},
	{expertName: "gemini_expert", executor: "gemini-cli", model: "gemini-3.1-pro-preview"},
}

// probeTimeout caps how long any single CLI's probe may run. Probes run
// in parallel, so total wall-clock is at most one slowest CLI.
const probeTimeout = 30 * time.Second

// initProbePrompt is the question the live probe sends. Short and
// model-agnostic; we only check the answer contains "OK" (case-
// insensitive) so we tolerate vendor preambles.
const initProbePrompt = "respond with the word OK"

// runInit implements `council init [--force]` (Task 7 of the v3 plan).
// It probes the registered CLIs in parallel, then writes
// $HOME/.config/council/default.yaml plus the prompt seeds the
// generated YAML references. Idempotent without --force: a pre-existing
// target file is left untouched and the command exits 0.
func runInit(argv []string, stdout, stderr io.Writer) int {
	fsArgs := flag.NewFlagSet("council init", flag.ContinueOnError)
	fsArgs.SetOutput(stderr)
	var force bool
	fsArgs.BoolVar(&force, "force", false, "Overwrite an existing default.yaml.")

	if err := fsArgs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitConfigError
	}
	if fsArgs.NArg() != 0 {
		fmt.Fprintln(stderr, "council init: unexpected positional argument")
		return exitConfigError
	}

	home, err := homeDir()
	if err != nil {
		fmt.Fprintf(stderr, "council init: resolve user home: %v\n", err)
		return exitConfigError
	}

	configDir := filepath.Join(home, ".config", "council")
	target := filepath.Join(configDir, "default.yaml")

	if !force {
		if _, err := os.Stat(target); err == nil {
			fmt.Fprintf(stdout, "council init: profile exists at %s (use --force to overwrite)\n", target)
			return exitOK
		} else if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "council init: stat %s: %v\n", target, err)
			return exitConfigError
		}
	}

	results := detectCLIs()

	verified := make([]candidate, 0, len(results))
	for _, r := range results {
		if r.ok {
			fmt.Fprintf(stdout, "verified: %s (%s, %.1fs)\n", r.cand.executor, r.cand.model, r.elapsed.Seconds())
			verified = append(verified, r.cand)
		} else {
			fmt.Fprintf(stdout, "skipped: %s (%s)\n", r.cand.executor, r.reason)
		}
	}

	if len(verified) == 0 {
		fmt.Fprintln(stderr, "council init: install at least one of: claude, codex, gemini (none verified)")
		return exitConfigError
	}

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "council init: mkdir %s: %v\n", configDir, err)
		return exitConfigError
	}
	if err := writePromptSeeds(configDir); err != nil {
		fmt.Fprintf(stderr, "council init: write prompts: %v\n", err)
		return exitConfigError
	}

	quorum := scaleQuorum(len(verified))
	yamlBody, err := buildInitYAML(verified, quorum)
	if err != nil {
		fmt.Fprintf(stderr, "council init: build yaml: %v\n", err)
		return exitConfigError
	}
	if err := os.WriteFile(target, yamlBody, 0o644); err != nil {
		fmt.Fprintf(stderr, "council init: write %s: %v\n", target, err)
		return exitConfigError
	}

	fmt.Fprintf(stdout, "wrote %s with %d experts, quorum %d\n", target, len(verified), quorum)
	return exitOK
}

// detectResult bundles a candidate with its probe outcome and elapsed
// wall-clock. Local to init.go; never escapes the package.
type detectResult struct {
	cand    candidate
	ok      bool
	reason  string
	elapsed time.Duration
}

// detectCLIs runs registry-presence + PATH-resolve + live-probe checks
// for every initCandidates entry in parallel. Each per-CLI probe gets
// its own context.WithTimeout(probeTimeout); a slow CLI cannot stall a
// fast one. Returns results in initCandidates order so the YAML keeps a
// stable expert sequence regardless of probe ordering.
func detectCLIs() []detectResult {
	results := make([]detectResult, len(initCandidates))
	var wg sync.WaitGroup
	for i, c := range initCandidates {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			ex, err := executor.Get(c.executor)
			if err != nil {
				results[i] = detectResult{cand: c, ok: false, reason: "executor not registered"}
				return
			}
			bin := ex.BinaryName()
			if _, err := initLookPath(bin); err != nil {
				results[i] = detectResult{cand: c, ok: false, reason: fmt.Sprintf("%s not on PATH", bin)}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			ok, reason := probe(ctx, ex, c.model)
			results[i] = detectResult{cand: c, ok: ok, reason: reason, elapsed: time.Since(start)}
		}()
	}
	wg.Wait()
	return results
}

// scaleQuorum returns the quorum value for n verified experts. The plan's
// rule: 3 verified → 2; 2 verified → 2; 1 verified → 1. Equivalent to
// min(2, n), kept as a tiny helper so the table-driven test stays
// self-documenting.
func scaleQuorum(n int) int {
	if n < 2 {
		return 1
	}
	return 2
}

// liveProbe spawns the executor with a short "respond with OK" prompt
// and reports whether stdout contained OK. Reason is empty on success;
// on failure it carries the spawn error or "no OK in output" depending
// on which assertion failed.
//
// liveProbe writes stdout/stderr to temp files because Executor.Execute
// requires real file paths; the files are removed before return.
func liveProbe(ctx context.Context, ex executor.Executor, model string) (bool, string) {
	tmp, err := os.MkdirTemp("", "council-init-probe-*")
	if err != nil {
		return false, fmt.Sprintf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(tmp)

	stdoutFile := filepath.Join(tmp, "stdout")
	stderrFile := filepath.Join(tmp, "stderr")

	req := executor.Request{
		Prompt:     initProbePrompt,
		Model:      model,
		Timeout:    probeTimeout,
		StdoutFile: stdoutFile,
		StderrFile: stderrFile,
	}
	if _, err := ex.Execute(ctx, req); err != nil {
		return false, fmt.Sprintf("probe failed: %v", err)
	}
	body, err := os.ReadFile(stdoutFile)
	if err != nil {
		return false, fmt.Sprintf("read stdout: %v", err)
	}
	if !strings.Contains(strings.ToUpper(string(body)), "OK") {
		return false, "no OK in probe output"
	}
	return true, ""
}

// writePromptSeeds copies the embedded prompts/{independent,peer-aware,
// ballot}.md to <configDir>/prompts/. The generated YAML references
// these by relative path, so they have to exist on disk for the loader
// to resolve them when council later loads the global config.
func writePromptSeeds(configDir string) error {
	promptsDir := filepath.Join(configDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", promptsDir, err)
	}
	for _, name := range []string{"independent.md", "peer-aware.md", "ballot.md"} {
		src := "prompts/" + name
		body, err := fs.ReadFile(defaults.FS, src)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", src, err)
		}
		dst := filepath.Join(promptsDir, name)
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

// initYAML mirrors pkg/config.yamlProfile but with only the fields
// council init produces. Defining a separate marshal-only struct here
// keeps the field-order deterministic (yaml.v3 marshals struct fields
// in declaration order) and avoids leaking marshal concerns into the
// loader package.
type initYAML struct {
	Version          int             `yaml:"version"`
	Name             string          `yaml:"name"`
	Experts          []initYAMLRole  `yaml:"experts"`
	Quorum           int             `yaml:"quorum"`
	MaxRetries       int             `yaml:"max_retries"`
	Rounds           int             `yaml:"rounds"`
	Round2PromptFile string          `yaml:"round_2_prompt_file"`
	Voting           initYAMLVoting  `yaml:"voting"`
}

type initYAMLRole struct {
	Name       string `yaml:"name"`
	Executor   string `yaml:"executor"`
	Model      string `yaml:"model"`
	PromptFile string `yaml:"prompt_file"`
	Timeout    string `yaml:"timeout"`
}

type initYAMLVoting struct {
	BallotPromptFile string `yaml:"ballot_prompt_file"`
	Timeout          string `yaml:"timeout"`
}

// buildInitYAML renders the profile YAML for the verified experts and
// computed quorum. Models, prompt paths, and timeouts are baked-in
// constants matching the plan's "Generated profile shape" sample.
func buildInitYAML(verified []candidate, quorum int) ([]byte, error) {
	roles := make([]initYAMLRole, 0, len(verified))
	for _, c := range verified {
		roles = append(roles, initYAMLRole{
			Name:       c.expertName,
			Executor:   c.executor,
			Model:      c.model,
			PromptFile: "prompts/independent.md",
			Timeout:    "300s",
		})
	}
	doc := initYAML{
		Version:          2,
		Name:             "default",
		Experts:          roles,
		Quorum:           quorum,
		MaxRetries:       1,
		Rounds:           2,
		Round2PromptFile: "prompts/peer-aware.md",
		Voting: initYAMLVoting{
			BallotPromptFile: "prompts/ballot.md",
			Timeout:          "300s",
		},
	}
	return yaml.Marshal(&doc)
}
