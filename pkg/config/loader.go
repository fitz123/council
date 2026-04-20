package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

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
type yamlRole struct {
	Name       string `yaml:"name,omitempty"`
	Executor   string `yaml:"executor"`
	Model      string `yaml:"model"`
	PromptFile string `yaml:"prompt_file"`
	Timeout    string `yaml:"timeout"`
}

type yamlProfile struct {
	Version    int        `yaml:"version"`
	Name       string     `yaml:"name"`
	Judge      yamlRole   `yaml:"judge"`
	Experts    []yamlRole `yaml:"experts"`
	Quorum     int        `yaml:"quorum"`
	MaxRetries int        `yaml:"max_retries"`
}

// Load resolves the profile with precedence:
//  1. <cwd>/.council/default.yaml
//  2. <home>/.config/council/default.yaml
//  3. embedded defaults (populated by pkg/config/embed.go in Task 8)
//
// It returns the parsed Profile, the source path ("embedded" for the embedded
// fallback), and any error.
func Load(cwd string) (*Profile, string, error) {
	local := filepath.Join(cwd, ".council", "default.yaml")
	if _, err := os.Stat(local); err == nil {
		p, err := LoadFile(local)
		return p, local, err
	}
	if home, err := userHomeDir(); err == nil {
		global := filepath.Join(home, ".config", "council", "default.yaml")
		if _, err := os.Stat(global); err == nil {
			p, err := LoadFile(global)
			return p, global, err
		}
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
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path %s: %w", path, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", abs, err)
	}
	defer f.Close()

	var y yamlProfile
	if err := decodeStrict(f, &y); err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}

	baseDir := filepath.Dir(abs)
	return buildProfile(&y, baseDir, readFileFS(os.DirFS(baseDir)), baseDir)
}

func decodeStrict(r io.Reader, out *yamlProfile) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// readFileFn reads the full contents of the prompt file at a path that is
// either absolute or relative to some loader-specific base. Abstracted so the
// embedded loader can reuse buildProfile.
type readFileFn func(absOrRel string) ([]byte, error)

func readFileFS(_ any) readFileFn {
	return func(p string) ([]byte, error) { return os.ReadFile(p) }
}

func buildProfile(y *yamlProfile, baseDir string, readFile readFileFn, resolveBase string) (*Profile, error) {
	if y.Version == 0 {
		return nil, fmt.Errorf("missing required field: version")
	}
	if y.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (expected 1)", y.Version)
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
	if len(y.Experts) == 0 {
		return nil, fmt.Errorf("missing required field: experts (must have at least one)")
	}

	judge, err := buildRole(&y.Judge, resolveBase, readFile, "judge")
	if err != nil {
		return nil, err
	}

	experts := make([]RoleConfig, 0, len(y.Experts))
	seen := map[string]bool{}
	for i := range y.Experts {
		r, err := buildRole(&y.Experts[i], resolveBase, readFile, fmt.Sprintf("experts[%d]", i))
		if err != nil {
			return nil, err
		}
		if r.Name == "" {
			return nil, fmt.Errorf("experts[%d]: missing required field: name", i)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("experts[%d]: duplicate expert name %q", i, r.Name)
		}
		seen[r.Name] = true
		experts = append(experts, *r)
	}

	_ = baseDir // reserved for future use (e.g. include resolution)
	return &Profile{
		Version:    y.Version,
		Name:       y.Name,
		Judge:      *judge,
		Experts:    experts,
		Quorum:     y.Quorum,
		MaxRetries: y.MaxRetries,
	}, nil
}

func buildRole(y *yamlRole, baseDir string, readFile readFileFn, label string) (*RoleConfig, error) {
	if y.Executor == "" {
		return nil, fmt.Errorf("%s: missing required field: executor", label)
	}
	if y.Model == "" {
		return nil, fmt.Errorf("%s: missing required field: model", label)
	}
	if y.PromptFile == "" {
		return nil, fmt.Errorf("%s: missing required field: prompt_file", label)
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
	if !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(baseDir, promptPath)
	}
	promptPath = filepath.Clean(promptPath)
	body, err := readFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("%s: read prompt_file %s: %w", label, promptPath, err)
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
