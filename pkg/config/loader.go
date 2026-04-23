package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// expertNameRE is the allowed character set for expert names. Names flow
// through filepath.Join (pkg/session.Session.RoundExpertDir) and appear in
// verdict.json and aggregate output, so anything outside `[a-zA-Z0-9_-]`
// risks path traversal or downstream text confusion.
// Must start with an alphanumeric to keep hidden-file shapes like ".hidden"
// out of the session folder.
var expertNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ErrNoConfig is returned by Load when no config file is found at any of the
// precedence locations and the embedded defaults are unavailable.
var ErrNoConfig = errors.New("no config file found (checked cwd/.council/default.yaml, ~/.config/council/default.yaml, embedded)")

// userHomeDir is var-indirect so tests can override the user home directory
// without touching the environment.
var userHomeDir = os.UserHomeDir

// SourceEmbedded is returned as the source path when Load resolves from the
// embedded defaults.
const SourceEmbedded = "embedded"

// yamlRole is the wire format for one role. Kept separate from RoleConfig so
// that yaml.v3's KnownFields rejects unknown keys at the wire layer while the
// in-memory Profile can carry computed fields (PromptBody, absolute paths).
//
// Either prompt_file or prompt_body must be set. prompt_file is the editable
// source-config form (path relative to the config file); prompt_body is the
// frozen snapshot form (verbatim bytes inlined into the YAML so reload is
// independent of the original prompt files). When both are present,
// prompt_body wins — this lets profile.snapshot.yaml record the original
// path for traceability while still being self-contained.
type yamlRole struct {
	Name       string `yaml:"name,omitempty"`
	Executor   string `yaml:"executor"`
	Model      string `yaml:"model"`
	PromptFile string `yaml:"prompt_file,omitempty"`
	PromptBody string `yaml:"prompt_body,omitempty"`
	Timeout    string `yaml:"timeout"`
}

// yamlVoting is the wire format for the v2 voting stage. ballot_prompt_file
// is required (or its inlined snapshot form ballot_prompt_body). timeout is
// optional — zero means downstream code picks its own default.
type yamlVoting struct {
	BallotPromptFile string `yaml:"ballot_prompt_file,omitempty"`
	BallotPromptBody string `yaml:"ballot_prompt_body,omitempty"`
	Timeout          string `yaml:"timeout,omitempty"`
}

type yamlProfile struct {
	Version          int        `yaml:"version"`
	Name             string     `yaml:"name"`
	Experts          []yamlRole `yaml:"experts"`
	Quorum           int        `yaml:"quorum"`
	MaxRetries       int        `yaml:"max_retries"`
	Rounds           int        `yaml:"rounds"`
	Round2PromptFile string     `yaml:"round_2_prompt_file,omitempty"`
	Round2PromptBody string     `yaml:"round_2_prompt_body,omitempty"`
	Voting           yamlVoting `yaml:"voting"`
	// Judge is accepted solely so a v1 profile (which always carried a
	// judge: block) gets the curated "version 1 not supported" migration
	// error instead of a cryptic "field judge not found" from KnownFields.
	// The field is otherwise ignored.
	Judge *yamlRole `yaml:"judge,omitempty"`
	// SessionNonce is only present on per-session snapshots written by
	// pkg/session; source-config YAML never sets it. Decoded here so strict
	// KnownFields parsing accepts snapshots round-tripped through LoadFile.
	// Profile itself does not carry the nonce — it belongs to session state,
	// not profile state. Use LoadSnapshot to get the nonce back.
	SessionNonce string `yaml:"session_nonce,omitempty"`
}

// Load resolves the profile with precedence:
//  1. <cwd>/.council/default.yaml
//  2. <home>/.config/council/default.yaml
//  3. embedded defaults
//
// It returns the parsed Profile, the source path ("embedded" for the embedded
// fallback), and any error.
func Load(cwd string) (*Profile, string, error) {
	local := filepath.Join(cwd, ".council", "default.yaml")
	switch _, err := os.Stat(local); {
	case err == nil:
		p, err := LoadFile(local)
		return p, local, err
	case !errors.Is(err, fs.ErrNotExist):
		// Permission denied, broken path component, etc. — surface
		// rather than silently falling through to a different source,
		// which could run a profile the operator didn't intend.
		return nil, "", fmt.Errorf("stat %s: %w", local, err)
	}
	home, homeErr := userHomeDir()
	switch {
	case homeErr == nil:
		global := filepath.Join(home, ".config", "council", "default.yaml")
		switch _, err := os.Stat(global); {
		case err == nil:
			p, err := LoadFile(global)
			return p, global, err
		case !errors.Is(err, fs.ErrNotExist):
			return nil, "", fmt.Errorf("stat %s: %w", global, err)
		}
	case errors.Is(homeErr, os.ErrNotExist):
		// No home dir at all (chroot, broken setup). Treat like "global
		// config absent" and fall through to embedded.
	default:
		// $HOME unset under sudo, a race on the passwd DB, or some other
		// UserHomeDir failure. Surface rather than silently running with
		// embedded defaults: the operator who keeps a profile at
		// ~/.config/council/default.yaml must see why it's being bypassed.
		return nil, "", fmt.Errorf("resolve user home: %w", homeErr)
	}
	p, err := loadFromEmbedded()
	if err != nil {
		return nil, "", err
	}
	return p, SourceEmbedded, nil
}

// LoadFile loads and validates a YAML profile at path. prompt_file values are
// resolved relative to the directory containing path; the resulting
// RoleConfig.PromptFile is always absolute.
func LoadFile(path string) (*Profile, error) {
	p, _, err := loadFileWithNonce(path)
	return p, err
}

// LoadSnapshot loads a per-session snapshot and returns the profile plus the
// session_nonce recorded at write time. Used by pkg/session.LoadExisting /
// resume (D14) to recover the nonce without re-generating it. Returns an
// empty nonce string if the file did not record one (e.g., a source-config
// YAML accidentally passed through this path).
func LoadSnapshot(path string) (*Profile, string, error) {
	return loadFileWithNonce(path)
}

func loadFileWithNonce(path string) (*Profile, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve config path %s: %w", path, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", fmt.Errorf("open config %s: %w", abs, err)
	}
	defer f.Close()

	var y yamlProfile
	if err := decodeStrict(f, &y); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", abs, err)
	}

	p, err := buildProfile(&y, filepath.Dir(abs), os.ReadFile)
	if err != nil {
		return nil, "", err
	}
	return p, y.SessionNonce, nil
}

func decodeStrict(r io.Reader, out *yamlProfile) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	// v2 profiles are single-document. A trailing `---\neffort: bogus`
	// would otherwise silently bypass KnownFields strictness, so require
	// EOF after the first document.
	var extra yaml.Node
	err := dec.Decode(&extra)
	if err == nil {
		return fmt.Errorf("unexpected additional YAML document (profiles must be single-document)")
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// readFileFn reads the full contents of the prompt file at a path that is
// either absolute or relative to some loader-specific base. Abstracted so the
// embedded loader can reuse buildProfile with fs.ReadFile against embed.FS.
type readFileFn func(absOrRel string) ([]byte, error)

func buildProfile(y *yamlProfile, resolveBase string, readFile readFileFn) (*Profile, error) {
	if y.Version == 0 {
		return nil, fmt.Errorf("missing required field: version")
	}
	if y.Version == 1 {
		return nil, fmt.Errorf("version 1 profiles are not supported by this binary (v2 debate engine); add `rounds: 2` and a `voting:` block with `ballot_prompt_file:`, then bump `version: 2`")
	}
	if y.Version != 2 {
		return nil, fmt.Errorf("unsupported version %d (expected 2)", y.Version)
	}
	// v2 retired the judge role (D15). A stray `judge:` block in a v2 profile
	// usually means an in-progress migration — reject loudly rather than
	// parsing successfully with the block silently ignored.
	if y.Judge != nil {
		return nil, fmt.Errorf("version 2 profiles must not include a judge block (the judge role was retired in favour of ballot voting)")
	}
	if y.Name == "" {
		return nil, fmt.Errorf("missing required field: name")
	}
	if y.Quorum < 1 {
		return nil, fmt.Errorf("quorum must be >= 1, got %d", y.Quorum)
	}
	if y.MaxRetries < 0 {
		return nil, fmt.Errorf("max_retries must be >= 0, got %d", y.MaxRetries)
	}
	if y.Rounds != 2 {
		return nil, fmt.Errorf("rounds must be 2 (v2 ships K=2 only; K=1 and K>=3 deferred to v3), got %d", y.Rounds)
	}
	// Debate requires >= 2 voices (R1 + R2 + voting all need at least two
	// candidates to compare). v1 ADR-0005 set the same floor for the judged
	// flow; v2 keeps it.
	if len(y.Experts) < 2 {
		return nil, fmt.Errorf("experts must have at least 2 entries (debate requires >= 2 voices), got %d", len(y.Experts))
	}

	experts := make([]RoleConfig, 0, len(y.Experts))
	// Dedup is case-insensitive: expert names become filesystem paths under
	// rounds/1/experts/<name>/, and macOS APFS (default case-insensitive) +
	// Windows NTFS would alias e.g. "critic" and "Critic" to the same directory.
	// Two goroutines racing mkdir + writes into one directory is silent
	// corruption, so reject the collision at validation time.
	seen := map[string]string{}
	for i := range y.Experts {
		r, err := buildRole(&y.Experts[i], resolveBase, readFile, fmt.Sprintf("experts[%d]", i))
		if err != nil {
			return nil, err
		}
		if r.Name == "" {
			return nil, fmt.Errorf("experts[%d]: missing required field: name", i)
		}
		if !expertNameRE.MatchString(r.Name) {
			return nil, fmt.Errorf("experts[%d]: invalid name %q (must match [a-zA-Z0-9][a-zA-Z0-9_-]*)", i, r.Name)
		}
		key := strings.ToLower(r.Name)
		if prior, ok := seen[key]; ok {
			if prior == r.Name {
				return nil, fmt.Errorf("experts[%d]: duplicate expert name %q", i, r.Name)
			}
			return nil, fmt.Errorf("experts[%d]: expert name %q collides with %q on case-insensitive filesystems (names map to filesystem paths)", i, r.Name, prior)
		}
		seen[key] = r.Name
		experts = append(experts, *r)
	}

	if y.Quorum > len(experts) {
		return nil, fmt.Errorf("quorum %d exceeds expert count %d (every run would fail)", y.Quorum, len(experts))
	}

	voting, err := buildVoting(&y.Voting, resolveBase, readFile)
	if err != nil {
		return nil, err
	}

	r2Prompt, err := buildRound2Prompt(y.Round2PromptFile, y.Round2PromptBody, resolveBase, readFile)
	if err != nil {
		return nil, err
	}

	return &Profile{
		Version:      y.Version,
		Name:         y.Name,
		Experts:      experts,
		Quorum:       y.Quorum,
		MaxRetries:   y.MaxRetries,
		Rounds:       y.Rounds,
		Round2Prompt: r2Prompt,
		Voting:       voting,
	}, nil
}

// buildRound2Prompt resolves the profile-level R2 role prompt (design §3.4's
// `[peer-aware role prompt]`). Required in v2 because R2 replaces the R1 role
// body with this one — omitting it would silently fall through to the R1
// prompt, which is what motivated peer-aware.md being created in the first
// place. round_2_prompt_body wins over round_2_prompt_file (same
// snapshot/source-config split as roles and voting).
func buildRound2Prompt(file, body, baseDir string, readFile readFileFn) (PromptSource, error) {
	if file == "" && body == "" {
		return PromptSource{}, fmt.Errorf("missing required field: round_2_prompt_file (or inline round_2_prompt_body)")
	}
	promptPath := file
	if promptPath != "" && !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(baseDir, promptPath)
	}
	if promptPath != "" {
		promptPath = filepath.Clean(promptPath)
	}
	var buf []byte
	if body != "" {
		buf = []byte(body)
	} else {
		var rerr error
		buf, rerr = readFile(promptPath)
		if rerr != nil {
			return PromptSource{}, fmt.Errorf("read round_2_prompt_file %s: %w", promptPath, rerr)
		}
	}
	if hasYAMLFrontmatter(buf) {
		source := promptPath
		if source == "" {
			source = "<inline round_2_prompt_body>"
		}
		return PromptSource{}, fmt.Errorf("round_2 prompt %s starts with YAML frontmatter", source)
	}
	return PromptSource{File: promptPath, Body: string(buf)}, nil
}

// hasYAMLFrontmatter reports whether body begins with a `---` line followed
// by a closing `---` line. Both terminators must end with a newline (so a
// raw `---` rule mid-body cannot trigger a false positive on the opening
// marker). Anything before the first non-whitespace character is ignored.
func hasYAMLFrontmatter(body []byte) bool {
	s := strings.TrimLeft(string(body), " \t\r\n")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return false
	}
	// Locate a closing `---` line after the opening one.
	rest := s[strings.IndexByte(s, '\n')+1:]
	for {
		nl := strings.IndexByte(rest, '\n')
		var line string
		if nl < 0 {
			line = rest
		} else {
			line = rest[:nl]
		}
		if strings.TrimRight(line, "\r") == "---" {
			return true
		}
		if nl < 0 {
			return false
		}
		rest = rest[nl+1:]
	}
}

func buildRole(y *yamlRole, baseDir string, readFile readFileFn, label string) (*RoleConfig, error) {
	if y.Executor == "" {
		return nil, fmt.Errorf("%s: missing required field: executor", label)
	}
	if y.Model == "" {
		return nil, fmt.Errorf("%s: missing required field: model", label)
	}
	if y.PromptFile == "" && y.PromptBody == "" {
		return nil, fmt.Errorf("%s: missing required field: prompt_file (or inline prompt_body)", label)
	}
	if y.Timeout == "" {
		return nil, fmt.Errorf("%s: missing required field: timeout", label)
	}
	d, err := time.ParseDuration(y.Timeout)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid timeout %q: %w", label, y.Timeout, err)
	}
	if d <= 0 {
		return nil, fmt.Errorf("%s: timeout must be > 0, got %s", label, d)
	}
	promptPath := y.PromptFile
	if promptPath != "" && !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(baseDir, promptPath)
	}
	if promptPath != "" {
		promptPath = filepath.Clean(promptPath)
	}
	// prompt_body wins over prompt_file when both are set: that's what makes
	// profile.snapshot.yaml a true frozen snapshot — the inlined body keeps
	// the recorded prompt_file purely informational.
	var body []byte
	if y.PromptBody != "" {
		body = []byte(y.PromptBody)
	} else {
		var rerr error
		body, rerr = readFile(promptPath)
		if rerr != nil {
			return nil, fmt.Errorf("%s: read prompt_file %s: %w", label, promptPath, rerr)
		}
	}
	// Reject YAML frontmatter at the top of a prompt body. v1 prompt files are
	// plain markdown; design/v1.md §16 F7 promises that a `---\nkey: value\n---`
	// header fails validation rather than being silently passed through to the
	// executor (where it would be interpreted as part of the role body and
	// confuse the LLM). v2 may introduce frontmatter under `version: 2`.
	if hasYAMLFrontmatter(body) {
		source := promptPath
		if source == "" {
			source = "<inline prompt_body>"
		}
		return nil, fmt.Errorf("%s: prompt body %s starts with YAML frontmatter, which is reserved for v2", label, source)
	}
	return &RoleConfig{
		Name:       y.Name,
		Executor:   y.Executor,
		Model:      y.Model,
		PromptFile: promptPath,
		Timeout:    d,
		PromptBody: string(body),
	}, nil
}

// buildVoting validates the voting block and loads the ballot prompt body so
// reload is independent of the original prompt file (matches RoleConfig
// semantics). ballot_prompt_body wins over ballot_prompt_file when both are
// present — same snapshot/source-config split as roles.
func buildVoting(y *yamlVoting, baseDir string, readFile readFileFn) (VotingConfig, error) {
	if y.BallotPromptFile == "" && y.BallotPromptBody == "" {
		return VotingConfig{}, fmt.Errorf("voting: missing required field: ballot_prompt_file (or inline ballot_prompt_body)")
	}
	promptPath := y.BallotPromptFile
	if promptPath != "" && !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(baseDir, promptPath)
	}
	if promptPath != "" {
		promptPath = filepath.Clean(promptPath)
	}
	var body []byte
	if y.BallotPromptBody != "" {
		body = []byte(y.BallotPromptBody)
	} else {
		var rerr error
		body, rerr = readFile(promptPath)
		if rerr != nil {
			return VotingConfig{}, fmt.Errorf("voting: read ballot_prompt_file %s: %w", promptPath, rerr)
		}
	}
	if hasYAMLFrontmatter(body) {
		source := promptPath
		if source == "" {
			source = "<inline ballot_prompt_body>"
		}
		return VotingConfig{}, fmt.Errorf("voting: ballot prompt %s starts with YAML frontmatter", source)
	}
	var d time.Duration
	if y.Timeout != "" {
		var err error
		d, err = time.ParseDuration(y.Timeout)
		if err != nil {
			return VotingConfig{}, fmt.Errorf("voting: invalid timeout %q: %w", y.Timeout, err)
		}
		if d <= 0 {
			return VotingConfig{}, fmt.Errorf("voting: timeout must be > 0, got %s", d)
		}
	}
	return VotingConfig{
		BallotPromptFile: promptPath,
		BallotPromptBody: string(body),
		Timeout:          d,
	}, nil
}
