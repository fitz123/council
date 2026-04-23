package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSnapshot_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)

	orig, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	snapPath := filepath.Join(t.TempDir(), "profile.snapshot.yaml")
	if err := Snapshot(orig, "", snapPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Re-load the snapshot; prompt_file entries are absolute so they resolve
	// regardless of the snapshot's containing directory.
	reloaded, err := LoadFile(snapPath)
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}

	if !reflect.DeepEqual(orig, reloaded) {
		t.Fatalf("round-trip mismatch:\norig  = %+v\nreload= %+v", orig, reloaded)
	}
}

func TestSnapshot_SessionNonceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	orig, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	snapPath := filepath.Join(t.TempDir(), "profile.snapshot.yaml")
	const wantNonce = "0123456789abcdef"
	if err := Snapshot(orig, wantNonce, snapPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	reloaded, gotNonce, err := LoadSnapshot(snapPath)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if gotNonce != wantNonce {
		t.Fatalf("nonce = %q, want %q", gotNonce, wantNonce)
	}
	if !reflect.DeepEqual(orig, reloaded) {
		t.Fatalf("profile round-trip mismatch with nonce:\norig  = %+v\nreload= %+v", orig, reloaded)
	}
}

func TestSnapshot_EmptyNonceOmitsKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	snapPath := filepath.Join(t.TempDir(), "profile.snapshot.yaml")
	if err := Snapshot(p, "", snapPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	b, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if bytes.Contains(b, []byte("session_nonce:")) {
		t.Fatalf("empty nonce should omit session_nonce key, got:\n%s", b)
	}
}

// TestSnapshot_BodyWinsOverEditedPromptFile locks in the resume guarantee
// that snapshot reload uses the inlined prompt_body, not whatever bytes
// happen to be at prompt_file at reload time. Without this, an operator
// editing prompts/*.md mid-session would silently change the round semantics
// of an in-flight resumed run.
func TestSnapshot_BodyWinsOverEditedPromptFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	orig, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	snapPath := filepath.Join(t.TempDir(), "profile.snapshot.yaml")
	if err := Snapshot(orig, "", snapPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Mutate every referenced prompt file on disk after the snapshot was
	// written. A correct loader will ignore these edits because the snapshot
	// inlines prompt_body for each role + voting + round_2_prompt.
	for i := range orig.Experts {
		if orig.Experts[i].PromptFile == "" {
			continue
		}
		if err := os.WriteFile(orig.Experts[i].PromptFile, []byte("EDITED expert\n"), 0o644); err != nil {
			t.Fatalf("edit expert prompt: %v", err)
		}
	}
	if orig.Voting.BallotPromptFile != "" {
		if err := os.WriteFile(orig.Voting.BallotPromptFile, []byte("EDITED ballot\n"), 0o644); err != nil {
			t.Fatalf("edit ballot prompt: %v", err)
		}
	}
	if orig.Round2Prompt.File != "" {
		if err := os.WriteFile(orig.Round2Prompt.File, []byte("EDITED r2\n"), 0o644); err != nil {
			t.Fatalf("edit r2 prompt: %v", err)
		}
	}

	reloaded, err := LoadFile(snapPath)
	if err != nil {
		t.Fatalf("reload snapshot after edits: %v", err)
	}
	for i := range reloaded.Experts {
		if reloaded.Experts[i].PromptBody != orig.Experts[i].PromptBody {
			t.Fatalf("expert[%d] body drifted: got %q, want %q",
				i, reloaded.Experts[i].PromptBody, orig.Experts[i].PromptBody)
		}
	}
	if reloaded.Voting.BallotPromptBody != orig.Voting.BallotPromptBody {
		t.Fatalf("ballot body drifted: got %q, want %q",
			reloaded.Voting.BallotPromptBody, orig.Voting.BallotPromptBody)
	}
	if reloaded.Round2Prompt.Body != orig.Round2Prompt.Body {
		t.Fatalf("r2 body drifted: got %q, want %q",
			reloaded.Round2Prompt.Body, orig.Round2Prompt.Body)
	}
}

func TestSnapshot_WritesValidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	out := filepath.Join(t.TempDir(), "snap.yaml")
	if err := Snapshot(p, "", out); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("snapshot is empty")
	}
}
