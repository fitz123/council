package config

import (
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
	if err := Snapshot(orig, snapPath); err != nil {
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

func TestSnapshot_WritesValidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeProfile(t, dir, validYAML)
	p, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	out := filepath.Join(t.TempDir(), "snap.yaml")
	if err := Snapshot(p, out); err != nil {
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
