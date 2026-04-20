package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadFromEmbedded_ProfileShape locks in the MVP default profile shape
// shipped in the binary — two experts (independent + critic) served by the
// claude-code executor, opus judge, quorum 1. A drift in defaults/default.yaml
// or defaults/prompts/*.md will surface here rather than at first real run.
func TestLoadFromEmbedded_ProfileShape(t *testing.T) {
	p, err := loadFromEmbedded()
	if err != nil {
		t.Fatalf("loadFromEmbedded: %v", err)
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
	if p.Judge.Executor != "claude-code" {
		t.Errorf("Judge.Executor = %q, want claude-code", p.Judge.Executor)
	}
	if p.Judge.Timeout != 300*time.Second {
		t.Errorf("Judge.Timeout = %s, want 300s", p.Judge.Timeout)
	}
	if !strings.Contains(p.Judge.PromptBody, "judge") {
		t.Errorf("Judge.PromptBody does not look like a judge prompt: %q", p.Judge.PromptBody)
	}

	if got := len(p.Experts); got != 2 {
		t.Fatalf("Experts = %d, want 2", got)
	}
	gotNames := []string{p.Experts[0].Name, p.Experts[1].Name}
	wantNames := []string{"independent", "critic"}
	for i, want := range wantNames {
		if gotNames[i] != want {
			t.Errorf("Experts[%d].Name = %q, want %q", i, gotNames[i], want)
		}
		if p.Experts[i].Executor != "claude-code" {
			t.Errorf("Experts[%d].Executor = %q, want claude-code", i, p.Experts[i].Executor)
		}
		if p.Experts[i].Model != "sonnet" {
			t.Errorf("Experts[%d].Model = %q, want sonnet", i, p.Experts[i].Model)
		}
		if p.Experts[i].Timeout != 180*time.Second {
			t.Errorf("Experts[%d].Timeout = %s, want 180s", i, p.Experts[i].Timeout)
		}
		if p.Experts[i].PromptBody == "" {
			t.Errorf("Experts[%d].PromptBody empty", i)
		}
	}
}

// TestLoadFromEmbedded_F7EarlyUnknownField is the F7 early gate: a local
// config with an unknown top-level field (e.g. `effort: bogus` reserved for
// v2) must be rejected by Load before the orchestrator ever spawns. cmd/
// council maps this failure to exit 1 (see cmd/council/main.go). We assert
// the loader half here; the exit-code half rides on the cmd/council test
// suite.
func TestLoadFromEmbedded_F7EarlyUnknownField(t *testing.T) {
	dir := t.TempDir()
	yaml := `version: 1
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
`
	cfgPath := writeProfile(t, dir, yaml)
	_, err := LoadFile(cfgPath)
	if err == nil {
		t.Fatal("want error for unknown top-level field, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "effort") {
		t.Errorf("error %q missing field name 'effort'", msg)
	}
}
