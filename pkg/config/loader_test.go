package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeProfile writes a .council/default.yaml under dir with the given YAML
// body plus three prompt files (judge.md, independent.md, critic.md).
func writeProfile(t *testing.T, dir string, yamlBody string) string {
	t.Helper()
	councilDir := filepath.Join(dir, ".council")
	promptsDir := filepath.Join(councilDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range map[string]string{
		"judge.md":       "you are the judge.\n",
		"independent.md": "you are independent.\n",
		"critic.md":      "you are critic.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write prompt %s: %v", name, err)
		}
	}
	cfg := filepath.Join(councilDir, "default.yaml")
	if err := os.WriteFile(cfg, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return cfg
}

const validYAML = `version: 1
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
  - name: critic
    executor: claude-code
    model: sonnet
    prompt_file: prompts/critic.md
    timeout: 180s
quorum: 1
max_retries: 1
`

func TestLoadFile_Valid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)

	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if p.Version != 1 {
		t.Errorf("Version = %d, want 1", p.Version)
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
	if p.Judge.Model != "opus" {
		t.Errorf("Judge.Model = %q, want opus", p.Judge.Model)
	}
	if p.Judge.Timeout != 300*time.Second {
		t.Errorf("Judge.Timeout = %s, want 300s", p.Judge.Timeout)
	}
	if !filepath.IsAbs(p.Judge.PromptFile) {
		t.Errorf("Judge.PromptFile not absolute: %s", p.Judge.PromptFile)
	}
	if p.Judge.PromptBody != "you are the judge.\n" {
		t.Errorf("Judge.PromptBody = %q", p.Judge.PromptBody)
	}
	if len(p.Experts) != 2 {
		t.Fatalf("Experts = %d, want 2", len(p.Experts))
	}
	if p.Experts[0].Name != "independent" || p.Experts[1].Name != "critic" {
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

func TestLoadFile_Errors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string // substring the error message must contain
	}{
		{
			name: "unknown top-level field",
			yaml: `version: 1
name: default
effort: bogus
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
			wantSub: "effort",
		},
		{
			name: "unknown per-expert field",
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
    memory: true
quorum: 1
max_retries: 0
`,
			wantSub: "memory",
		},
		{
			name: "missing version",
			yaml: `name: default
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
			wantSub: "version",
		},
		{
			name: "missing experts",
			yaml: `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/judge.md
  timeout: 300s
experts: []
quorum: 1
max_retries: 0
`,
			wantSub: "experts",
		},
		{
			name: "missing expert name",
			yaml: `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/judge.md
  timeout: 300s
experts:
  - executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
`,
			wantSub: "name",
		},
		{
			name: "duplicate expert name",
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
  - name: independent
    executor: claude-code
    model: sonnet
    prompt_file: prompts/critic.md
    timeout: 180s
quorum: 1
max_retries: 0
`,
			wantSub: "duplicate",
		},
		{
			name: "bad timeout",
			yaml: `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/judge.md
  timeout: notaduration
experts:
  - name: independent
    executor: claude-code
    model: sonnet
    prompt_file: prompts/independent.md
    timeout: 180s
quorum: 1
max_retries: 0
`,
			wantSub: "timeout",
		},
		{
			name: "zero quorum",
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
quorum: 0
max_retries: 0
`,
			wantSub: "quorum",
		},
		{
			name: "bad YAML syntax",
			yaml: `version: 1
name: default
judge: [this, is, wrong
experts:
`,
			wantSub: "parse",
		},
		{
			name: "unsupported version",
			yaml: `version: 2
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
			wantSub: "version",
		},
		{
			name: "missing prompt file on disk",
			yaml: `version: 1
name: default
judge:
  executor: claude-code
  model: opus
  prompt_file: prompts/does-not-exist.md
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
			wantSub: "does-not-exist",
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
	for _, n := range []string{"judge.md", "independent.md", "critic.md"} {
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
	for _, n := range []string{"judge.md", "independent.md", "critic.md"} {
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

// TestLoad_FallsThroughToEmbedded covers the precedence-chain terminus: with
// neither a cwd-local nor a user-global config file on disk, Load must resolve
// the embedded profile and flag its source as SourceEmbedded. This replaces
// the earlier ErrNoConfig expectation, which only held while Task 8 had not
// yet wired up the //go:embed fallback.
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
	if len(p.Experts) != 2 {
		t.Errorf("Experts = %d, want 2", len(p.Experts))
	}
}
