package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile is a test helper that creates parent dirs then writes a file.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// touchFile creates an empty file (optionally creating parents).
func touchFile(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, "")
}

// makeSessionDir returns the path to a fresh session-like folder under
// <root>/.council/sessions/<name>/. Callers then populate it to match the
// scenario they want FindIncomplete to see.
func makeSessionDir(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, ".council", "sessions", name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// sessionsRoot returns <root>/.council/sessions — the path FindIncomplete is
// invoked against.
func sessionsRoot(root string) string {
	return filepath.Join(root, ".council", "sessions")
}

func TestFindIncomplete_NoFolders(t *testing.T) {
	root := t.TempDir()
	_, err := FindIncomplete(sessionsRoot(root))
	if !errors.Is(err, ErrNoResumableSession) {
		t.Fatalf("err = %v, want ErrNoResumableSession", err)
	}
}

func TestFindIncomplete_RootDoneSkipped(t *testing.T) {
	root := t.TempDir()
	sess := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
	// Has stage progress AND a root .done: finality wins.
	touchFile(t, filepath.Join(sess, "rounds", "1", "experts", "A", ".done"))
	touchFile(t, filepath.Join(sess, ".done"))
	_, err := FindIncomplete(sessionsRoot(root))
	if !errors.Is(err, ErrNoResumableSession) {
		t.Fatalf("err = %v, want ErrNoResumableSession (root .done is final)", err)
	}
}

func TestFindIncomplete_FinalVerdictStatusesSkipped(t *testing.T) {
	statuses := []string{
		"ok",
		"no_consensus",
		"quorum_failed_round_1",
		"quorum_failed_round_2",
		"injection_suspected_in_question",
		"config_error",
		"error",
	}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			sess := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
			touchFile(t, filepath.Join(sess, "rounds", "1", "experts", "A", ".done"))
			writeFile(t, filepath.Join(sess, "verdict.json"),
				`{"status": "`+status+`"}`)
			_, err := FindIncomplete(sessionsRoot(root))
			if !errors.Is(err, ErrNoResumableSession) {
				t.Fatalf("status=%s: err = %v, want ErrNoResumableSession", status, err)
			}
		})
	}
}

func TestFindIncomplete_NoStageDoneSkipped(t *testing.T) {
	root := t.TempDir()
	sess := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
	// Folder exists, profile.snapshot.yaml+question.md even, but no stage
	// progressed past subprocess completion: nothing to resume from.
	writeFile(t, filepath.Join(sess, "question.md"), "q")
	writeFile(t, filepath.Join(sess, "profile.snapshot.yaml"), "version: 2\n")
	_, err := FindIncomplete(sessionsRoot(root))
	if !errors.Is(err, ErrNoResumableSession) {
		t.Fatalf("err = %v, want ErrNoResumableSession (no stage .done)", err)
	}
}

func TestFindIncomplete_StageDoneNoVerdictReturned(t *testing.T) {
	root := t.TempDir()
	sess := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
	touchFile(t, filepath.Join(sess, "rounds", "1", "experts", "A", ".done"))
	got, err := FindIncomplete(sessionsRoot(root))
	if err != nil {
		t.Fatalf("FindIncomplete: %v", err)
	}
	if got != sess {
		t.Fatalf("got = %q, want %q", got, sess)
	}
}

func TestFindIncomplete_InterruptedVerdictReturned(t *testing.T) {
	root := t.TempDir()
	sess := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
	// SIGINT-mid-R2: R1 finished, partial verdict.json with status "interrupted".
	touchFile(t, filepath.Join(sess, "rounds", "1", "experts", "A", ".done"))
	touchFile(t, filepath.Join(sess, "rounds", "1", "experts", "B", ".done"))
	writeFile(t, filepath.Join(sess, "verdict.json"),
		`{"status": "interrupted"}`)
	got, err := FindIncomplete(sessionsRoot(root))
	if err != nil {
		t.Fatalf("FindIncomplete: %v", err)
	}
	if got != sess {
		t.Fatalf("got = %q, want %q", got, sess)
	}
}

func TestFindIncomplete_NewestWins(t *testing.T) {
	root := t.TempDir()
	older := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
	newer := makeSessionDir(t, root, "2026-04-05T00-00-00Z-d-e-f")
	touchFile(t, filepath.Join(older, "rounds", "1", "experts", "A", ".done"))
	touchFile(t, filepath.Join(newer, "rounds", "1", "experts", "A", ".done"))
	got, err := FindIncomplete(sessionsRoot(root))
	if err != nil {
		t.Fatalf("FindIncomplete: %v", err)
	}
	if got != newer {
		t.Fatalf("got = %q, want %q (newer)", got, newer)
	}
}

func TestFindIncomplete_SessionsRootMissing(t *testing.T) {
	// A fresh cwd with no .council folder at all: treat as "nothing to
	// resume" rather than a crash.
	root := t.TempDir()
	_, err := FindIncomplete(sessionsRoot(root))
	if !errors.Is(err, ErrNoResumableSession) {
		t.Fatalf("err = %v, want ErrNoResumableSession", err)
	}
}

// TestLoadExisting_RestoresSessionState verifies that a session on disk
// round-trips through LoadExisting with ID, Path, and Nonce intact — the
// three fields orchestrator.Run reads off *session.Session.
func TestLoadExisting_RestoresSessionState(t *testing.T) {
	cwd := t.TempDir()
	id := NewID(time.Now())
	const wantNonce = "feedfacecafebabe"
	s, err := Create(cwd, id, testProfile(t), wantNonce, "what is 2+2?")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := LoadExisting(s.Path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if got.Path != s.Path {
		t.Errorf("Path = %q, want %q", got.Path, s.Path)
	}
	if got.Nonce != wantNonce {
		t.Errorf("Nonce = %q, want %q", got.Nonce, wantNonce)
	}
}

func TestLoadExisting_MissingSnapshotIsError(t *testing.T) {
	root := t.TempDir()
	sess := makeSessionDir(t, root, "2026-04-01T00-00-00Z-a-b-c")
	_, err := LoadExisting(sess)
	if err == nil {
		t.Fatalf("LoadExisting: expected error for missing profile.snapshot.yaml, got nil")
	}
}

func TestLoadQuestion_ReadsQuestionMD(t *testing.T) {
	cwd := t.TempDir()
	id := NewID(time.Now())
	s, err := Create(cwd, id, testProfile(t), "", "what is 2+2?")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := LoadQuestion(s.Path)
	if err != nil {
		t.Fatalf("LoadQuestion: %v", err)
	}
	if got != "what is 2+2?" {
		t.Errorf("got = %q, want %q", got, "what is 2+2?")
	}
}
