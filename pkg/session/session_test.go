package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/council/pkg/config"
)

// testProfile returns a minimal valid v2 Profile suitable for handing to
// Session.Create. Real callers get this from pkg/config; tests construct one
// inline so they don't depend on disk fixtures.
//
// v2-shaped (version: 2, rounds: 2, voting block with inline ballot_prompt_body)
// so round-tripping through LoadSnapshot works without touching the filesystem
// for prompt files.
func testProfile(t *testing.T) *config.Profile {
	t.Helper()
	return &config.Profile{
		Version: 2,
		Name:    "default",
		Experts: []config.RoleConfig{
			{Name: "independent", Executor: "claude-code", Model: "sonnet", PromptBody: "i body", Timeout: 180 * time.Second},
			{Name: "critic", Executor: "claude-code", Model: "sonnet", PromptBody: "c body", Timeout: 180 * time.Second},
		},
		Quorum:       1,
		MaxRetries:   1,
		Rounds:       2,
		Round2Prompt: config.PromptSource{Body: "peer-aware body\n"},
		Voting: config.VotingConfig{
			BallotPromptBody: "VOTE: <label>\n",
			Timeout:          180 * time.Second,
		},
	}
}

func TestCreate_FolderShape(t *testing.T) {
	cwd := t.TempDir()
	id := NewID(time.Now())
	s, err := Create(cwd, id, testProfile(t), "0123456789abcdef", "what is 2+2?")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Nonce != "0123456789abcdef" {
		t.Errorf("Session.Nonce = %q, want %q", s.Nonce, "0123456789abcdef")
	}
	if s.ID != id {
		t.Errorf("Session.ID = %q, want %q", s.ID, id)
	}
	wantPath := filepath.Join(cwd, ".council", "sessions", id)
	if s.Path != wantPath {
		t.Errorf("Session.Path = %q, want %q", s.Path, wantPath)
	}

	// Required artifacts.
	for _, f := range []string{
		"question.md",
		"profile.snapshot.yaml",
		"rounds/1/experts",
	} {
		if _, err := os.Stat(filepath.Join(s.Path, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}

	got, err := os.ReadFile(filepath.Join(s.Path, "question.md"))
	if err != nil {
		t.Fatalf("read question.md: %v", err)
	}
	if string(got) != "what is 2+2?" {
		t.Errorf("question.md = %q, want %q", got, "what is 2+2?")
	}
}

// TestCreate_ParallelDistinctFolders is the F3 precondition: three Creates
// running concurrently in the same cwd must produce three distinct, non-
// colliding session folders. NewID's petname suffix gives ~10^9 combinations,
// so even at the same wall-clock instant the IDs will differ.
func TestCreate_ParallelDistinctFolders(t *testing.T) {
	cwd := t.TempDir()
	const n = 3
	now := time.Now()
	results := make([]*Session, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := NewID(now)
			results[i], errs[i] = Create(cwd, id, testProfile(t), "", "q")
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, s := range results {
		if errs[i] != nil {
			t.Fatalf("Create[%d]: %v", i, errs[i])
		}
		if seen[s.Path] {
			t.Fatalf("duplicate session path: %s", s.Path)
		}
		seen[s.Path] = true
	}
}

// TestCreate_RejectsExistingSessionDir guards the exclusive-creation
// invariant: if the session root already exists (stale folder, or NewID
// collision), Create must return os.ErrExist rather than overwriting
// question.md / profile.snapshot.yaml in the old session.
func TestCreate_RejectsExistingSessionDir(t *testing.T) {
	cwd := t.TempDir()
	id := NewID(time.Now())
	sessionPath := filepath.Join(cwd, ".council", "sessions", id)
	if err := os.MkdirAll(sessionPath, 0o755); err != nil {
		t.Fatalf("pre-create session dir: %v", err)
	}
	if _, err := Create(cwd, id, testProfile(t), "", "q"); err == nil {
		t.Fatalf("Create: expected error for existing session dir, got nil")
	} else if !errors.Is(err, os.ErrExist) {
		t.Fatalf("Create: err = %v, want os.ErrExist", err)
	}
}

func TestRoundExpertDir(t *testing.T) {
	s := &Session{ID: "x", Path: "/tmp/abc"}
	if got, want := s.RoundExpertDir(1, "A"), "/tmp/abc/rounds/1/experts/A"; got != want {
		t.Errorf("RoundExpertDir(1,A) = %q, want %q", got, want)
	}
	if got, want := s.RoundExpertDir(2, "C"), "/tmp/abc/rounds/2/experts/C"; got != want {
		t.Errorf("RoundExpertDir(2,C) = %q, want %q", got, want)
	}
}

func TestTouchDone_Success(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	if err := s.TouchDone(dir); err != nil {
		t.Fatalf("TouchDone: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ".done"))
	if err != nil {
		t.Fatalf("stat .done: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf(".done size = %d, want 0", info.Size())
	}
}

// TestCreate_NoncePersistedInSnapshot is the task-4 invariant: the
// session nonce Create was given is recorded in profile.snapshot.yaml and
// can be recovered by LoadSnapshot. Resume (D14) depends on this.
func TestCreate_NoncePersistedInSnapshot(t *testing.T) {
	cwd := t.TempDir()
	id := NewID(time.Now())
	const wantNonce = "feedfacecafebabe"
	s, err := Create(cwd, id, testProfile(t), wantNonce, "q")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snapPath := filepath.Join(s.Path, "profile.snapshot.yaml")
	// Raw inspection: the nonce must be present as a top-level YAML key.
	raw, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !strings.Contains(string(raw), "session_nonce: "+wantNonce) {
		t.Fatalf("snapshot does not record session_nonce:\n%s", raw)
	}
	// Structural reload: config.LoadSnapshot must surface the nonce
	// alongside the profile so resume can re-use it without regenerating.
	_, gotNonce, err := config.LoadSnapshot(snapPath)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if gotNonce != wantNonce {
		t.Fatalf("LoadSnapshot nonce = %q, want %q", gotNonce, wantNonce)
	}
}

// TestLoadSnapshot_EmptyNonce guards the non-debate caller contract: if
// Create was given an empty nonce, LoadSnapshot returns the empty string
// rather than a zero-value surprise (e.g., spurious "0000..." fencing).
func TestLoadSnapshot_EmptyNonce(t *testing.T) {
	cwd := t.TempDir()
	id := NewID(time.Now())
	s, err := Create(cwd, id, testProfile(t), "", "q")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, gotNonce, err := config.LoadSnapshot(filepath.Join(s.Path, "profile.snapshot.yaml"))
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if gotNonce != "" {
		t.Fatalf("LoadSnapshot nonce = %q, want empty", gotNonce)
	}
}

func TestTouchDone_MissingDir(t *testing.T) {
	s := &Session{ID: "x", Path: "/tmp"}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := s.TouchDone(missing)
	if err == nil {
		t.Fatalf("TouchDone on missing dir should return error, got nil")
	}
	// Ensure error message references the missing path so debugging is easy.
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %v should mention missing path %q", err, missing)
	}
}
