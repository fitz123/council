package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/fitz123/council/pkg/config"
)

// ErrNoResumableSession is returned by FindIncomplete when no session under
// the given sessions root is in a resumable state. `council resume` maps this
// to exit 1 ("no_resumable_session") so the operator sees a clear signal
// rather than a generic config error.
var ErrNoResumableSession = errors.New("session: no resumable session found")

// finalVerdictStatuses enumerate the verdict.json statuses that mark a run as
// terminal — `council resume` must skip any session whose verdict carries one
// of these. "interrupted" is deliberately absent: SIGINT-mid-run writes an
// interrupted verdict, and that is exactly the state resume is designed to
// pick up. Kept in sync with docs/design/v2.md §4 exit-code table.
var finalVerdictStatuses = map[string]bool{
	"ok":                              true,
	"no_consensus":                    true,
	"quorum_failed_round_1":           true,
	"quorum_failed_round_2":           true,
	"injection_suspected_in_question": true,
	"config_error":                    true,
	// "error" is a terminal catch-all written by orchestrator when an
	// unrecoverable I/O or setup error aborts the run (see
	// pkg/orchestrator/orchestrator.go). finalizeAndWrite also drops the
	// root .done marker on that path, so the .done-based predicate would
	// already skip such sessions — listing "error" here closes the
	// defense-in-depth gap where .done write itself fails.
	"error": true,
}

// FindIncomplete scans sessionsRoot (typically <cwd>/.council/sessions/) for
// the newest session that is still resumable per D14's finality predicate:
//
//   - root-level .done marker present → final, skip
//   - verdict.json.status ∈ finalVerdictStatuses → final, skip
//   - no stage .done markers anywhere under rounds/ → nothing progressed, skip
//   - otherwise → resumable candidate
//
// Multiple resumable candidates are broken by picking the newest by lex-sort
// on session directory name (the ISO-timestamp prefix makes the ordering
// chronologically equivalent). Returns ErrNoResumableSession when no
// candidate qualifies or when sessionsRoot itself does not exist.
func FindIncomplete(sessionsRoot string) (string, error) {
	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNoResumableSession
		}
		return "", fmt.Errorf("read sessions root %s: %w", sessionsRoot, err)
	}
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(sessionsRoot, e.Name())
		if isResumable(path) {
			candidates = append(candidates, path)
		}
	}
	if len(candidates) == 0 {
		return "", ErrNoResumableSession
	}
	sort.Strings(candidates)
	return candidates[len(candidates)-1], nil
}

// IsResumable applies the D14 finality predicate to a single session folder.
// A single blocked predicate skips; all predicates passed means "resume can
// pick this session up." Exposed so `council resume --session <id>` can gate
// an explicitly named session on the same predicate FindIncomplete applies to
// the implicit newest-incomplete search — otherwise an operator could mutate a
// completed session's verdict/output by naming it directly.
func IsResumable(sessionPath string) bool {
	return isResumable(sessionPath)
}

// isResumable is the unexported body behind IsResumable. Kept as the
// intra-package workhorse so tests and FindIncomplete do not have to go
// through the exported wrapper.
func isResumable(sessionPath string) bool {
	if _, err := os.Stat(filepath.Join(sessionPath, ".done")); err == nil {
		return false
	}
	if status, ok := readVerdictStatus(sessionPath); ok {
		if finalVerdictStatuses[status] {
			return false
		}
	}
	return hasStageDone(sessionPath)
}

// readVerdictStatus returns the verdict.json status field if present and
// parseable, plus an ok flag. Parse errors and missing files return ok=false
// — a malformed verdict is treated as "no final signal" so the stage-.done
// predicate is the tiebreaker.
func readVerdictStatus(sessionPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(sessionPath, "verdict.json"))
	if err != nil {
		return "", false
	}
	var v struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", false
	}
	return v.Status, true
}

// hasStageDone reports whether any subprocess stage under rounds/ completed
// cleanly, which is the progress signal that makes a session resumable.
// Walks rounds/** looking for a file literally named ".done".
func hasStageDone(sessionPath string) bool {
	roundsDir := filepath.Join(sessionPath, "rounds")
	found := false
	_ = filepath.WalkDir(roundsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == ".done" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// LoadExisting rebuilds a *Session pointing at an already-materialised session
// folder. ID is taken from the folder name (which orchestrator.Run feeds to
// debate.AssignLabels, so anonymization auto-rederives), Nonce from the
// persisted profile.snapshot.yaml. The profile itself is recovered via
// config.LoadSnapshot and the question by LoadQuestion; LoadExisting
// intentionally keeps the Session struct lean so callers decide what extra
// state they need.
func LoadExisting(sessionPath string) (*Session, error) {
	abs, err := filepath.Abs(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("resolve session path %s: %w", sessionPath, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", abs)
	}
	snapPath := filepath.Join(abs, "profile.snapshot.yaml")
	_, nonce, err := config.LoadSnapshot(snapPath)
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", snapPath, err)
	}
	// Refuse to resume a session whose snapshot lacks the per-session
	// nonce: prompt.CheckForgery skips the nonce-substring check when
	// nonce=="" (ADR-0008 D11), so silently proceeding would degrade
	// forgery protection to regex-only. Force a hard failure so the
	// operator notices the corrupted/manually-edited snapshot.
	if nonce == "" {
		return nil, fmt.Errorf("snapshot %s: missing session_nonce", snapPath)
	}
	return &Session{ID: filepath.Base(abs), Path: abs, Nonce: nonce}, nil
}

// LoadQuestion returns the question.md contents of an existing session
// folder. Split out of LoadExisting so callers that already have the session
// state can pull the question lazily without re-reading the snapshot.
func LoadQuestion(sessionPath string) (string, error) {
	path := filepath.Join(sessionPath, "question.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), nil
}
