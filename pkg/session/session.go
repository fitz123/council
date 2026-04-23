package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/fitz123/council/pkg/config"
)

// Session represents one council run's on-disk folder. It does not own any
// running subprocesses — the orchestrator owns those and uses Session purely
// as a path/IO helper.
//
// Nonce is the 16-hex value used to fence every LLM-sourced string before
// it flows back into a downstream prompt (ADR-0008 D11). It is copied from
// the Create call / LoadSnapshot read so callers do not need to thread it
// through separately.
type Session struct {
	ID    string
	Path  string
	Nonce string
}

// Create allocates the session folder under <cwd>/.council/sessions/<id>/ and
// writes the initial fixed artifacts: question.md and profile.snapshot.yaml.
// The rounds/1/experts/ subtree is created so per-stage directories can be
// made by the orchestrator.
//
// nonce is the 16-hex session nonce from pkg/debate.GenerateNonce. It is
// recorded in profile.snapshot.yaml under the session_nonce key so
// `council resume` (D14) can re-derive the original fencing nonce without
// re-running the randomness.
//
// Per-expert subdirectories under rounds/<r>/experts/<label>/ are NOT
// created here — that is the orchestrator's responsibility once it knows
// which experts the profile has.
func Create(cwd, id string, profile *config.Profile, nonce, question string) (*Session, error) {
	root := filepath.Join(cwd, ".council", "sessions", id)
	// Ensure the ancestors exist, then create the session root exclusively
	// so a collision (NewID clash, or a stale folder from a prior run) is
	// surfaced as os.ErrExist instead of silently overwriting artifacts.
	// Callers (cmd/council) retry NewID on ErrExist.
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(root), err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	expertsRoot := filepath.Join(root, "rounds", "1", "experts")
	if err := os.MkdirAll(expertsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", expertsRoot, err)
	}
	if err := os.WriteFile(filepath.Join(root, "question.md"), []byte(question), 0o644); err != nil {
		return nil, fmt.Errorf("write question.md: %w", err)
	}
	if err := config.Snapshot(profile, nonce, filepath.Join(root, "profile.snapshot.yaml")); err != nil {
		return nil, fmt.Errorf("write profile.snapshot.yaml: %w", err)
	}
	return &Session{ID: id, Path: root, Nonce: nonce}, nil
}

// RoundExpertDir returns the absolute path of one expert's round-N stage
// directory under rounds/<round>/experts/<label>/. The directory is not
// created here — callers run os.MkdirAll before subprocess work. label is
// the anonymized single-letter token assigned by pkg/debate.AssignLabels.
func (s *Session) RoundExpertDir(round int, label string) string {
	return filepath.Join(s.Path, "rounds", strconv.Itoa(round), "experts", label)
}

// TouchDone writes an empty .done marker inside dir. Only the orchestrator
// calls this, after cmd.Wait returns and the stdout file has been synced. It
// must NOT be written by child processes or by the executor package; its
// presence on disk means "this stage completed cleanly".
//
// Returns an error if dir does not exist (as opposed to silently creating it):
// a missing dir at TouchDone time means the orchestrator skipped a step
// upstream and we want that to surface as a failure.
func (s *Session) TouchDone(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	donePath := filepath.Join(dir, ".done")
	f, err := os.OpenFile(donePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", donePath, err)
	}
	return f.Close()
}
