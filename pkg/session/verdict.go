package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// VerdictV1 is the canonical machine-readable run index written to
// verdict.json at session end. Field order, JSON tags, and types must match
// docs/design/v1.md §6 byte-for-byte; the snapshot test in verdict_test.go
// gates this contract.
//
// version stays 1 for the entire v1 release line. v2 bumps to 2 with a new
// Go type to keep the schemas separable.
type VerdictV1 struct {
	Version         int     `json:"version"`
	SessionID       string  `json:"session_id"`
	SessionPath     string  `json:"session_path"`
	Profile         string  `json:"profile"`
	Question        string  `json:"question"`
	Answer          string  `json:"answer"`
	Status          string  `json:"status"`
	Rounds          []Round `json:"rounds"`
	StartedAt       string  `json:"started_at"`
	EndedAt         string  `json:"ended_at"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// Round captures one debate round. v1 always emits a single round at index 0;
// v2 will append rounds 2..N.
type Round struct {
	Experts []ExpertResult `json:"experts"`
	Judge   JudgeResult    `json:"judge"`
}

// ExpertResult is the per-expert summary recorded in verdict.json. Status
// values: "ok" | "failed" | "interrupted".
type ExpertResult struct {
	Name            string  `json:"name"`
	Executor        string  `json:"executor"`
	Model           string  `json:"model"`
	Status          string  `json:"status"`
	ExitCode        int     `json:"exit_code"`
	Retries         int     `json:"retries"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// JudgeResult is the judge stage summary. The judge has no Status field
// because top-level VerdictV1.Status carries that signal: "ok" implies the
// judge succeeded; "judge_failed" implies it did not.
type JudgeResult struct {
	Executor        string  `json:"executor"`
	Model           string  `json:"model"`
	ExitCode        int     `json:"exit_code"`
	Retries         int     `json:"retries"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// writeSyncCloser is the subset of *os.File the verdict writer needs. Tests
// substitute a spy implementation via openTempFile to assert call ordering
// (Write -> Sync -> Close -> rename).
type writeSyncCloser interface {
	io.Writer
	Sync() error
	Close() error
}

var (
	openTempFile = func(path string, flag int, perm os.FileMode) (writeSyncCloser, error) {
		return os.OpenFile(path, flag, perm)
	}
	renameFn = os.Rename
)

// WriteVerdict serializes v as indented JSON and writes it atomically to
// <session>/verdict.json: write to verdict.json.tmp with O_EXCL, fsync, close,
// then rename. Concurrent readers see either nothing, the previous file, or
// the new file — never a partial write.
//
// The O_EXCL on the .tmp file guarantees a single writer per session: if a
// stale .tmp exists (e.g. from a crashed prior run), this returns an error
// containing os.ErrExist and the operator must clear the stale file. We
// prefer surfacing that over silently overwriting and masking a concurrency
// bug.
func (s *Session) WriteVerdict(v *VerdictV1) error {
	buf, err := marshalVerdict(v)
	if err != nil {
		return fmt.Errorf("marshal verdict: %w", err)
	}
	tmpPath := filepath.Join(s.Path, "verdict.json.tmp")
	finalPath := filepath.Join(s.Path, "verdict.json")
	f, err := openTempFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpPath, err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := renameFn(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

// marshalVerdict serializes v with two-space indent and HTML escaping
// disabled. HTML escaping would mangle '<', '>', '&' inside question/answer
// bodies — verdict.json is not consumed by browsers and we want the bytes to
// match what the user wrote.
func marshalVerdict(v *VerdictV1) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
