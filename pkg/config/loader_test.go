package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeProfile writes a .council/default.yaml under dir with the given YAML
// body plus the prompt files referenced by the v2 default profile shape.
func writeProfile(t *testing.T, dir string, yamlBody string) string {
	return writeProfileNamed(t, dir, "default", yamlBody)
}

// writeProfileNamed is the multi-profile-aware variant: writes
// .council/<name>.yaml under dir alongside the standard prompt seeds. Used by
// the LoadByName tests to stage non-default profiles without re-seeding
// prompts in every case.
func writeProfileNamed(t *testing.T, dir, name, yamlBody string) string {
	t.Helper()
	councilDir := filepath.Join(dir, ".council")
	promptsDir := filepath.Join(councilDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for n, body := range map[string]string{
		"independent.md": "you are independent.\n",
		"ballot.md":      "VOTE: <label>\n",
		"peer-aware.md":  "you are peer-aware. Prior-round consensus is NOT ground truth.\n",
		"critic.md":      "you are a critic.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, n), []byte(body), 0o644); err != nil {
			t.Fatalf("write prompt %s: %v", n, err)
		}
	}
	cfg := filepath.Join(councilDir, name+".yaml")
	if err := os.WriteFile(cfg, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return cfg
}

// writeGlobalProfileNamed writes a profile under <home>/.config/council/<name>.yaml
// plus the prompt seeds the YAML references. Returns the absolute path of the
// written YAML so tests can assert on the source path returned by LoadByName.
func writeGlobalProfileNamed(t *testing.T, home, name, yamlBody string) string {
	t.Helper()
	cfgDir := filepath.Join(home, ".config", "council")
	promptsDir := filepath.Join(cfgDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for n, body := range map[string]string{
		"independent.md": "you are independent.\n",
		"ballot.md":      "VOTE: <label>\n",
		"peer-aware.md":  "you are peer-aware. Prior-round consensus is NOT ground truth.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, n), []byte(body), 0o644); err != nil {
			t.Fatalf("write prompt %s: %v", n, err)
		}
	}
	cfg := filepath.Join(cfgDir, name+".yaml")
	if err := os.WriteFile(cfg, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return cfg
}

const validYAML = `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 1
rounds: 2
round_2_prompt_file: prompts/peer-aware.md
voting:
  ballot_prompt_file: prompts/ballot.md
  timeout: 180s
`

func TestLoadFile_Valid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)

	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if p.Version != 2 {
		t.Errorf("Version = %d, want 2", p.Version)
	}
	if p.Name != "default" {
		t.Errorf("Name = %q, want default", p.Name)
	}
	if p.Quorum != 1 {
		t.Errorf("Quorum = %d, want 1", p.Quorum)
	}
	if p.MaxRetries != 1 {
		t.Errorf("MaxRetries = %d, want 1", p.MaxRetries)
	}
	if p.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2", p.Rounds)
	}
	if p.Round2Prompt.File == "" {
		t.Error("Round2Prompt.File is empty")
	}
	if !strings.Contains(p.Round2Prompt.Body, "peer-aware") {
		t.Errorf("Round2Prompt.Body does not look peer-aware: %q", p.Round2Prompt.Body)
	}
	if p.Voting.BallotPromptFile == "" {
		t.Error("Voting.BallotPromptFile is empty")
	}
	if !filepath.IsAbs(p.Voting.BallotPromptFile) {
		t.Errorf("Voting.BallotPromptFile not absolute: %s", p.Voting.BallotPromptFile)
	}
	if p.Voting.BallotPromptBody != "VOTE: <label>\n" {
		t.Errorf("Voting.BallotPromptBody = %q", p.Voting.BallotPromptBody)
	}
	if p.Voting.Timeout != 180*time.Second {
		t.Errorf("Voting.Timeout = %s, want 180s", p.Voting.Timeout)
	}
	if len(p.Experts) != 2 {
		t.Fatalf("Experts = %d, want 2", len(p.Experts))
	}
	if p.Experts[0].Name != "expert_1" || p.Experts[1].Name != "expert_2" {
		t.Errorf("Experts names = %q,%q", p.Experts[0].Name, p.Experts[1].Name)
	}
	for _, e := range p.Experts {
		if e.Timeout != 180*time.Second {
			t.Errorf("expert %s timeout = %s", e.Name, e.Timeout)
		}
		if !filepath.IsAbs(e.PromptFile) {
			t.Errorf("expert %s PromptFile not absolute: %s", e.Name, e.PromptFile)
		}
		if e.PromptBody == "" {
			t.Errorf("expert %s PromptBody empty", e.Name)
		}
	}
}

// TestLoadFile_SingleExpertAccepted — v3 relaxed the experts-floor from
// >=2 to >=1 (loader.go:231) so `council init` can write a 1-expert
// profile when only one CLI is authed on the host. The validator must
// accept it; downstream debate degenerates to "trust the one survivor".
func TestLoadFile_SingleExpertAccepted(t *testing.T) {
	dir := t.TempDir()
	yaml := `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
round_2_prompt_file: prompts/peer-aware.md
voting:
  ballot_prompt_file: prompts/ballot.md
  timeout: 180s
`
	cfgPath := writeProfile(t, dir, yaml)
	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile(single expert) = %v, want nil", err)
	}
	if len(p.Experts) != 1 {
		t.Errorf("Experts count = %d, want 1", len(p.Experts))
	}
	if p.Quorum != 1 {
		t.Errorf("Quorum = %d, want 1", p.Quorum)
	}
}

// TestLoadFile_VotingTimeoutOptional confirms that a profile may omit
// voting.timeout — Profile.Voting.Timeout is then zero and downstream code
// applies its own default. The plan only mandates ballot_prompt_file as
// required for the voting block.
func TestLoadFile_VotingTimeoutOptional(t *testing.T) {
	dir := t.TempDir()
	yaml := `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 1
rounds: 2
round_2_prompt_file: prompts/peer-aware.md
voting:
  ballot_prompt_file: prompts/ballot.md
`
	cfgPath := writeProfile(t, dir, yaml)
	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if p.Voting.Timeout != 0 {
		t.Errorf("Voting.Timeout = %s, want 0 (unset)", p.Voting.Timeout)
	}
	if p.Voting.BallotPromptBody == "" {
		t.Error("Voting.BallotPromptBody empty")
	}
}

func TestLoadFile_Errors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string // substring the error message must contain
	}{
		{
			name: "unknown top-level field",
			yaml: `version: 2
name: default
effort: bogus
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "effort",
		},
		{
			name: "unknown per-expert field",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
    memory: true
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "memory",
		},
		{
			name: "unknown voting field",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
  policy: instant_runoff
`,
			wantSub: "policy",
		},
		{
			name: "missing version",
			yaml: `name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "version",
		},
		{
			name: "missing experts",
			yaml: `version: 2
name: default
experts: []
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "at least 1",
		},
		{
			name: "missing expert name",
			yaml: `version: 2
name: default
experts:
  - executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "name",
		},
		{
			name: "duplicate expert name",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/critic.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "duplicate",
		},
		{
			name: "case-insensitive expert name collision",
			yaml: `version: 2
name: default
experts:
  - name: critic
    executor: claude-code
    model: sonnet
    prompt_file: prompts/critic.md
    timeout: 180s
  - name: Critic
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "case-insensitive",
		},
		{
			name: "bad timeout",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: notaduration
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "timeout",
		},
		{
			name: "zero quorum",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 0
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "quorum",
		},
		{
			name: "quorum exceeds expert count",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 5
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "exceeds expert count",
		},
		{
			name: "bad YAML syntax",
			yaml: `version: 2
name: default
experts: [this, is, wrong
`,
			wantSub: "parse",
		},
		{
			name: "version 1 (migration error)",
			yaml: `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/judge.md
  timeout: 300s
experts:
  - name: independent
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
`,
			wantSub: "version 1 profiles are not supported",
		},
		{
			name: "version 1 error mentions rounds and voting",
			yaml: `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/judge.md
  timeout: 300s
experts:
  - name: independent
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
`,
			wantSub: "rounds: 2",
		},
		{
			name: "v2 profile with stray judge block",
			yaml: `version: 2
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/independent.md
  timeout: 300s
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "judge",
		},
		{
			name: "unsupported version 3",
			yaml: `version: 3
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "unsupported version 3",
		},
		{
			name: "extra YAML document bypass attempt",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
---
effort: bogus
`,
			wantSub: "additional YAML document",
		},
		{
			name: "missing prompt file on disk",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/does-not-exist.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "does-not-exist",
		},
		{
			name: "rounds = 0 (missing)",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "rounds must be 2",
		},
		{
			name: "rounds = 1 (K=1 deferred to v3)",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 1
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "rounds must be 2",
		},
		{
			name: "rounds = 3 (K>=3 deferred to v3)",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 3
voting:
  ballot_prompt_file: prompts/ballot.md
`,
			wantSub: "rounds must be 2",
		},
		{
			name: "missing voting block entirely",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
`,
			wantSub: "ballot_prompt_file",
		},
		{
			name: "voting present but ballot_prompt_file missing",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  timeout: 60s
`,
			wantSub: "ballot_prompt_file",
		},
		{
			name: "voting timeout invalid duration",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/ballot.md
  timeout: notaduration
`,
			wantSub: "voting: invalid timeout",
		},
		{
			name: "voting ballot prompt missing on disk",
			yaml: `version: 2
name: default
experts:
  - name: expert_1
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
  - name: expert_2
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
rounds: 2
voting:
  ballot_prompt_file: prompts/no-such-ballot.md
`,
			wantSub: "no-such-ballot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := writeProfile(t, dir, tc.yaml)
			_, err := LoadFile(cfgPath)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestLoadFile_RejectsYAMLFrontmatter covers F7b — an expert prompt body that
// begins with `---\nfoo: bar\n---` is rejected at load time. The error must
// mention the offending prompt file so operators can locate it without
// grepping.
func TestLoadFile_RejectsYAMLFrontmatter(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	// Overwrite the shared independent.md with frontmatter-bearing content.
	bad := filepath.Join(dir, ".council", "prompts", "independent.md")
	if err := os.WriteFile(bad, []byte("---\nfoo: bar\n---\nrest of body\n"), 0o644); err != nil {
		t.Fatalf("write bad prompt: %v", err)
	}
	_, err := LoadFile(cfgPath)
	if err == nil {
		t.Fatalf("LoadFile: expected frontmatter rejection, got nil")
	}
	if !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("error %q should mention frontmatter", err)
	}
	if !strings.Contains(err.Error(), "independent.md") {
		t.Fatalf("error %q should name the offending prompt file", err)
	}
}

// TestLoadFile_RejectsYAMLFrontmatterInBallot mirrors the role check for the
// ballot prompt: a frontmatter-laden ballot.md is rejected at load time.
func TestLoadFile_RejectsYAMLFrontmatterInBallot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	bad := filepath.Join(dir, ".council", "prompts", "ballot.md")
	if err := os.WriteFile(bad, []byte("---\nfoo: bar\n---\nVOTE: <label>\n"), 0o644); err != nil {
		t.Fatalf("write bad ballot: %v", err)
	}
	_, err := LoadFile(cfgPath)
	if err == nil {
		t.Fatal("LoadFile: expected frontmatter rejection for ballot, got nil")
	}
	if !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("error %q should mention frontmatter", err)
	}
}

// TestHasYAMLFrontmatter exercises the detector on edge cases the loader
// relies on: bare body, leading whitespace, mid-body `---` rule (must NOT
// trigger), CRLF line endings, missing closing fence.
func TestHasYAMLFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain body", "you are an expert.\n", false},
		{"valid frontmatter", "---\nfoo: bar\n---\nbody\n", true},
		{"frontmatter with leading blank", "\n\n---\nx: 1\n---\n", true},
		{"crlf frontmatter", "---\r\nx: 1\r\n---\r\n", true},
		{"mid-body horizontal rule", "intro\n\n---\nnot frontmatter\n", false},
		{"no closing fence", "---\nopen but never closed\n", false},
		{"three dashes no newline", "---", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasYAMLFrontmatter([]byte(tc.in))
			if got != tc.want {
				t.Fatalf("hasYAMLFrontmatter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLoad_PrecedenceLocalOverGlobal(t *testing.T) {
	local := t.TempDir()
	global := t.TempDir()
	// Seed a global config (should be ignored because local wins).
	if err := os.MkdirAll(filepath.Join(global, ".config", "council"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, local, validYAML)
	// Write a distinguishably different global (name=global) — if Load picks
	// it, the test detects it.
	globalCfgDir := filepath.Join(global, ".config", "council")
	globalPromptsDir := filepath.Join(globalCfgDir, "prompts")
	if err := os.MkdirAll(globalPromptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"independent.md", "ballot.md", "peer-aware.md"} {
		_ = os.WriteFile(filepath.Join(globalPromptsDir, n), []byte("global\n"), 0o644)
	}
	globalYAML := strings.Replace(validYAML, "name: default", "name: global", 1)
	if err := os.WriteFile(filepath.Join(globalCfgDir, "default.yaml"), []byte(globalYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return global, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	p, src, err := Load(local)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "default" {
		t.Errorf("Name = %q, want default (local should win)", p.Name)
	}
	if !strings.Contains(src, local) {
		t.Errorf("source %q does not reference local cwd %q", src, local)
	}
}

func TestLoad_PrecedenceGlobalWhenNoLocal(t *testing.T) {
	local := t.TempDir() // no .council here
	global := t.TempDir()
	globalCfgDir := filepath.Join(global, ".config", "council")
	globalPromptsDir := filepath.Join(globalCfgDir, "prompts")
	if err := os.MkdirAll(globalPromptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"independent.md", "ballot.md", "peer-aware.md"} {
		_ = os.WriteFile(filepath.Join(globalPromptsDir, n), []byte("global body\n"), 0o644)
	}
	globalYAML := strings.Replace(validYAML, "name: default", "name: global", 1)
	if err := os.WriteFile(filepath.Join(globalCfgDir, "default.yaml"), []byte(globalYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return global, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	p, src, err := Load(local)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "global" {
		t.Errorf("Name = %q, want global", p.Name)
	}
	if !strings.Contains(src, global) {
		t.Errorf("source %q does not reference global home %q", src, global)
	}
}

// TestLoad_UserHomeDirErrorIsSurfaced covers the case where userHomeDir
// returns an unexpected error (e.g. $HOME unset under sudo). A silent
// fall-through to embedded would hide the operator's real problem — their
// ~/.config/council/default.yaml is being bypassed without explanation.
// The loader must return the error instead.
func TestLoad_UserHomeDirErrorIsSurfaced(t *testing.T) {
	local := t.TempDir() // no .council here

	oldHome := userHomeDir
	sentinel := errors.New("home lookup failed")
	userHomeDir = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { userHomeDir = oldHome })

	_, _, err := Load(local)
	if err == nil {
		t.Fatalf("Load: expected error when userHomeDir fails, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Load error = %v, want to wrap %v", err, sentinel)
	}
}

// TestLoad_FallsThroughToEmbedded covers the precedence-chain terminus: with
// neither a cwd-local nor a user-global config file on disk, Load must
// resolve the embedded profile and flag its source as SourceEmbedded.
func TestLoad_FallsThroughToEmbedded(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir() // empty

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	p, src, err := Load(local)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src != SourceEmbedded {
		t.Errorf("source = %q, want %q", src, SourceEmbedded)
	}
	if p.Name != "default" {
		t.Errorf("Name = %q, want default", p.Name)
	}
	if p.Version != 2 {
		t.Errorf("Version = %d, want 2", p.Version)
	}
	if p.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2", p.Rounds)
	}
}

// --- LoadByName: name-mode tests ---

// cheapYAML is a distinguishably-named profile body so tests can assert that
// LoadByName actually loaded the cheap.yaml file (not, say, embedded default
// or a sibling default.yaml). Same shape as validYAML otherwise.
const cheapYAML = `version: 2
name: cheap
experts:
  - name: expert_1
    executor: claude-code
    model: haiku
    prompt_file: prompts/independent.md
    timeout: 60s
quorum: 1
max_retries: 0
rounds: 2
round_2_prompt_file: prompts/peer-aware.md
voting:
  ballot_prompt_file: prompts/ballot.md
  timeout: 60s
`

// TestLoadByName_NameMode_LocalWins — when both <cwd>/.council/cheap.yaml and
// <home>/.config/council/cheap.yaml exist, the local copy wins (same
// precedence rule that Load already enforces for default.yaml).
func TestLoadByName_NameMode_LocalWins(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir()

	writeProfileNamed(t, local, "cheap", cheapYAML)
	// Distinguishable global so a wrong pick is detectable.
	globalYAML := strings.Replace(cheapYAML, "name: cheap", "name: cheap_global", 1)
	writeGlobalProfileNamed(t, home, "cheap", globalYAML)

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	p, src, err := LoadByName(local, "cheap")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if p.Name != "cheap" {
		t.Errorf("Name = %q, want cheap (local should win)", p.Name)
	}
	if !strings.Contains(src, local) {
		t.Errorf("source %q does not reference local cwd %q", src, local)
	}
	if !strings.HasSuffix(src, "cheap.yaml") {
		t.Errorf("source %q does not end in cheap.yaml", src)
	}
}

// TestLoadByName_NameMode_HomeFallback — when only the home file exists,
// LoadByName picks it up and the source path points at the home location.
func TestLoadByName_NameMode_HomeFallback(t *testing.T) {
	local := t.TempDir() // no .council here
	home := t.TempDir()
	cfgPath := writeGlobalProfileNamed(t, home, "cheap", cheapYAML)

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	p, src, err := LoadByName(local, "cheap")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if p.Name != "cheap" {
		t.Errorf("Name = %q, want cheap", p.Name)
	}
	if src != cfgPath {
		t.Errorf("source = %q, want %q", src, cfgPath)
	}
}

// TestLoadByName_NameMode_NoEmbedFallbackForNonDefault — the embedded fallback
// is reserved for `-p default` (zero-config UX). A named profile with no file
// on disk must error out, not silently substitute the embedded default — the
// user explicitly named "cheap"; falling back to default would be surprising.
func TestLoadByName_NameMode_NoEmbedFallbackForNonDefault(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir()

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	_, _, err := LoadByName(local, "cheap")
	if err == nil {
		t.Fatalf("LoadByName: expected error for missing non-default profile, got nil")
	}
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("LoadByName error = %v, want to wrap ErrProfileNotFound", err)
	}
	msg := err.Error()
	for _, want := range []string{"cheap", local, home} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing substring %q (so the user can see what was checked)", msg, want)
		}
	}
}

// TestLoadByName_NameMode_DefaultStillEmbeds — `-p default` (the historical
// default flag value) keeps the embedded fallback so a fresh install with no
// config file still runs.
func TestLoadByName_NameMode_DefaultStillEmbeds(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir()

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	p, src, err := LoadByName(local, "default")
	if err != nil {
		t.Fatalf("LoadByName(default): %v", err)
	}
	if src != SourceEmbedded {
		t.Errorf("source = %q, want %q", src, SourceEmbedded)
	}
	if p.Name != "default" {
		t.Errorf("Name = %q, want default", p.Name)
	}
}

// TestLoadByName_NameMode_InvalidName — names that fail profileNameRE are
// rejected before any filesystem access. Covers path-traversal shapes,
// hidden-file shapes, leading dashes, spaces, and the empty string.
func TestLoadByName_NameMode_InvalidName(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir()

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	cases := []string{
		"",
		"..",
		".hidden",
		"-leading-dash",
		"foo bar",
		"foo:bar",
		`foo\bar`,
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := LoadByName(local, name)
			if err == nil {
				t.Fatalf("LoadByName(%q): expected error, got nil", name)
			}
			if !errors.Is(err, ErrInvalidProfileName) {
				t.Errorf("LoadByName(%q) error = %v, want to wrap ErrInvalidProfileName", name, err)
			}
		})
	}
}

// --- LoadByName: path-mode tests ---

// TestLoadByName_PathMode_AbsolutePath — an absolute path bypasses the name
// precedence walk entirely and loads the file directly.
func TestLoadByName_PathMode_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfileNamed(t, dir, "cheap", cheapYAML)

	if !filepath.IsAbs(cfgPath) {
		t.Fatalf("test setup: writeProfileNamed returned non-absolute %q", cfgPath)
	}

	p, src, err := LoadByName(t.TempDir(), cfgPath)
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if p.Name != "cheap" {
		t.Errorf("Name = %q, want cheap", p.Name)
	}
	if src != cfgPath {
		t.Errorf("source = %q, want %q", src, cfgPath)
	}
}

// TestLoadByName_PathMode_RelativePath — a relative path with a slash is
// path-mode and loads relative to whatever the OS resolves it against. We
// chdir so the relative path resolves deterministically.
func TestLoadByName_PathMode_RelativePath(t *testing.T) {
	dir := t.TempDir()
	writeProfileNamed(t, dir, "cheap", cheapYAML)

	t.Chdir(dir)

	p, _, err := LoadByName(dir, "./.council/cheap.yaml")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if p.Name != "cheap" {
		t.Errorf("Name = %q, want cheap", p.Name)
	}
}

// TestLoadByName_PathMode_BareYAMLSuffix — a bare filename ending in .yaml is
// path-mode (no slash, but the suffix is the trigger). Resolves against cwd.
func TestLoadByName_PathMode_BareYAMLSuffix(t *testing.T) {
	dir := t.TempDir()
	// Drop the file directly in dir (no .council/ prefix) so the bare path
	// "cheap.yaml" resolves there.
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, body := range map[string]string{
		"independent.md": "you are independent.\n",
		"ballot.md":      "VOTE: <label>\n",
		"peer-aware.md":  "you are peer-aware. Prior-round consensus is NOT ground truth.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, n), []byte(body), 0o644); err != nil {
			t.Fatalf("write prompt %s: %v", n, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cheap.yaml"), []byte(cheapYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(dir)

	p, _, err := LoadByName(dir, "cheap.yaml")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if p.Name != "cheap" {
		t.Errorf("Name = %q, want cheap", p.Name)
	}
}

// TestLoadByName_PathMode_Missing — a missing path is a hard error, no
// embedded fallback. The user passed an explicit path; silently substituting
// embedded defaults would mask a typo.
func TestLoadByName_PathMode_Missing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.yaml")

	_, _, err := LoadByName(dir, missing)
	if err == nil {
		t.Fatalf("LoadByName: expected error for missing path, got nil")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("error %q does not name the missing path", err)
	}
}

// TestLoad_StillCallsLoadByNameDefault — `Load(cwd)` is the back-compat
// wrapper for `LoadByName(cwd, "default")`. Lock the contract so a future
// refactor doesn't accidentally drop the embedded-fallback semantics callers
// implicitly depend on.
func TestLoad_StillCallsLoadByNameDefault(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir()

	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	pLoad, srcLoad, errLoad := Load(local)
	pName, srcName, errName := LoadByName(local, "default")
	if errLoad != nil || errName != nil {
		t.Fatalf("Load=%v LoadByName=%v", errLoad, errName)
	}
	if srcLoad != srcName {
		t.Errorf("Load src=%q, LoadByName(default) src=%q", srcLoad, srcName)
	}
	if pLoad.Name != pName.Name {
		t.Errorf("Load Name=%q, LoadByName(default) Name=%q", pLoad.Name, pName.Name)
	}
}
