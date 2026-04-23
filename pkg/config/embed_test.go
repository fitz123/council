package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadFromEmbedded_ProfileShape locks in the v2 default profile shape
// shipped in the binary — three independent experts (matching design D6),
// no judge, debate K=2, voting via prompts/ballot.md. A drift in
// defaults/default.yaml or defaults/prompts/*.md will surface here rather
// than at first real run.
func TestLoadFromEmbedded_ProfileShape(t *testing.T) {
	p, err := loadFromEmbedded()
	if err != nil {
		t.Fatalf("loadFromEmbedded: %v", err)
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
	if !strings.Contains(p.Round2Prompt.Body, "peer") {
		t.Errorf("Round2Prompt.Body does not look like the peer-aware prompt: %q", p.Round2Prompt.Body)
	}

	if p.Voting.BallotPromptFile == "" {
		t.Error("Voting.BallotPromptFile is empty")
	}
	if !strings.Contains(p.Voting.BallotPromptBody, "VOTE") {
		t.Errorf("Voting.BallotPromptBody does not look like a ballot prompt: %q", p.Voting.BallotPromptBody)
	}
	if p.Voting.Timeout != 180*time.Second {
		t.Errorf("Voting.Timeout = %s, want 180s", p.Voting.Timeout)
	}

	wantNames := []string{"expert_1", "expert_2", "expert_3"}
	if got := len(p.Experts); got != len(wantNames) {
		t.Fatalf("Experts = %d, want %d", got, len(wantNames))
	}
	for i, want := range wantNames {
		if p.Experts[i].Name != want {
			t.Errorf("Experts[%d].Name = %q, want %q", i, p.Experts[i].Name, want)
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
		if !strings.Contains(p.Experts[i].PromptBody, "independent expert") {
			t.Errorf("Experts[%d].PromptBody does not look like the independent prompt", i)
		}
	}
}

// TestLoadFromEmbedded_F7EarlyUnknownField is the F7 early gate: a local
// config with an unknown top-level field (e.g. `effort: bogus`) must be
// rejected by Load before the orchestrator ever spawns. cmd/council maps
// this failure to exit 1 (see cmd/council/main.go).
func TestLoadFromEmbedded_F7EarlyUnknownField(t *testing.T) {
	dir := t.TempDir()
	yaml := `version: 2
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
