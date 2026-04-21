package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// canonicalVerdict mirrors the example in docs/design/v1.md §6 byte-for-byte
// once marshaled. Drift on any non-time/non-duration field breaks the schema
// contract and fails this test (see TestVerdictV1_BytesExact below).
func canonicalVerdict() *VerdictV1 {
	return &VerdictV1{
		Version:     1,
		SessionID:   "2026-04-19T17-02-14Z-fizzy-jingling-quokka",
		SessionPath: "./.council/sessions/2026-04-19T17-02-14Z-fizzy-jingling-quokka",
		Profile:     "default",
		Question:    "what is 2+2?",
		Answer:      "Four. (Sample synthesis.)",
		Status:      "ok",
		Rounds: []Round{{
			Experts: []ExpertResult{
				{Name: "independent", Executor: "claude-code", Model: "sonnet", Status: "ok", ExitCode: 0, Retries: 0, DurationSeconds: 17.3},
				{Name: "critic", Executor: "claude-code", Model: "sonnet", Status: "ok", ExitCode: 0, Retries: 0, DurationSeconds: 19.1},
			},
			Judge: JudgeResult{Executor: "claude-code", Model: "opus", ExitCode: 0, Retries: 0, DurationSeconds: 14.1},
		}},
		StartedAt:       "2026-04-19T17:02:14Z",
		EndedAt:         "2026-04-19T17:02:45Z",
		DurationSeconds: 31.4,
	}
}

func TestVerdictV1_BytesExact(t *testing.T) {
	got, err := marshalVerdict(canonicalVerdict())
	if err != nil {
		t.Fatalf("marshalVerdict: %v", err)
	}
	want, err := os.ReadFile("testdata/verdict_canonical.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verdict bytes drift from canonical fixture\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestWriteVerdict_OEXCL(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	// Pre-create a stale .tmp so O_EXCL forces an error.
	tmp := filepath.Join(dir, "verdict.json.tmp")
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	err := s.WriteVerdict(canonicalVerdict())
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected os.ErrExist, got %v", err)
	}
}

// TestWriteVerdict_SyncBeforeRename injects a spy implementation of
// writeSyncCloser so we can assert the order of operations is
// Write -> Sync -> Close -> rename. If the writer ever renames before
// fsync, a torn write on crash would corrupt verdict.json.
func TestWriteVerdict_SyncBeforeRename(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}

	var ops []string
	origOpen := openTempFile
	origRename := renameFn
	t.Cleanup(func() {
		openTempFile = origOpen
		renameFn = origRename
	})
	openTempFile = func(path string, flag int, perm os.FileMode) (writeSyncCloser, error) {
		f, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &spyFile{File: f, ops: &ops}, nil
	}
	renameFn = func(oldPath, newPath string) error {
		ops = append(ops, "rename")
		return os.Rename(oldPath, newPath)
	}

	if err := s.WriteVerdict(canonicalVerdict()); err != nil {
		t.Fatalf("WriteVerdict: %v", err)
	}

	want := []string{"write", "sync", "close", "rename"}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("op order = %v, want %v", ops, want)
	}
}

// TestWriteVerdict_ConcurrentReader spawns a reader loop racing the writer
// and asserts the reader never sees a partial JSON document — only either no
// file or a fully valid one. This validates the atomic-rename contract.
func TestWriteVerdict_ConcurrentReader(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	verdict := canonicalVerdict()

	var stop atomic.Bool
	var partial atomic.Bool
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		path := filepath.Join(dir, "verdict.json")
		for !stop.Load() {
			b, err := os.ReadFile(path)
			if err != nil {
				continue // file may not exist yet — fine
			}
			var v VerdictV1
			if err := json.Unmarshal(b, &v); err != nil {
				partial.Store(true)
				return
			}
		}
	}()

	// Write enough times to give the reader a real chance of catching a
	// partial state if the writer were not atomic. Each iteration removes
	// the prior verdict.json so O_EXCL on .tmp does not block (it should
	// never block here in any case because Rename is atomic and we always
	// end with no .tmp on disk).
	for i := 0; i < 200; i++ {
		if err := s.WriteVerdict(verdict); err != nil {
			stop.Store(true)
			done.Wait()
			t.Fatalf("WriteVerdict iter %d: %v", i, err)
		}
	}
	stop.Store(true)
	done.Wait()
	if partial.Load() {
		t.Fatal("reader observed a partial verdict.json — atomic-rename invariant violated")
	}

	// Final check: the file on disk equals the canonical bytes.
	got, err := os.ReadFile(filepath.Join(dir, "verdict.json"))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	want, err := os.ReadFile("testdata/verdict_canonical.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("final verdict.json differs from canonical fixture")
	}
}

// spyFile records the order of Write/Sync/Close calls without changing
// behavior. Used by TestWriteVerdict_SyncBeforeRename above.
type spyFile struct {
	*os.File
	ops *[]string
}

func (s *spyFile) Write(p []byte) (int, error) {
	*s.ops = append(*s.ops, "write")
	return s.File.Write(p)
}

func (s *spyFile) Sync() error {
	*s.ops = append(*s.ops, "sync")
	return s.File.Sync()
}

func (s *spyFile) Close() error {
	*s.ops = append(*s.ops, "close")
	return s.File.Close()
}

// flakyFile is a writeSyncCloser whose Write/Sync/Close methods return a
// configurable error. Used to drive cleanup paths in TestWriteVerdict_*Error.
type flakyFile struct {
	*os.File
	failWrite error
	failSync  error
	failClose error
}

func (f *flakyFile) Write(p []byte) (int, error) {
	if f.failWrite != nil {
		return 0, f.failWrite
	}
	return f.File.Write(p)
}

func (f *flakyFile) Sync() error {
	if f.failSync != nil {
		return f.failSync
	}
	return f.File.Sync()
}

func (f *flakyFile) Close() error {
	if f.failClose != nil {
		_ = f.File.Close()
		return f.failClose
	}
	return f.File.Close()
}

func withFlakyOpen(t *testing.T, ff *flakyFile) {
	t.Helper()
	orig := openTempFile
	t.Cleanup(func() { openTempFile = orig })
	openTempFile = func(path string, flag int, perm os.FileMode) (writeSyncCloser, error) {
		f, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		ff.File = f
		return ff, nil
	}
}

// Each error path leaves no stale .tmp on disk: the writer always cleans up
// after itself so the next session-write call won't trip O_EXCL.
func TestWriteVerdict_WriteError(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	withFlakyOpen(t, &flakyFile{failWrite: errors.New("boom-write")})

	err := s.WriteVerdict(canonicalVerdict())
	if err == nil || err.Error() == "" {
		t.Fatalf("want error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "verdict.json.tmp")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale verdict.json.tmp left on disk after Write failure")
	}
}

func TestWriteVerdict_SyncError(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	withFlakyOpen(t, &flakyFile{failSync: errors.New("boom-sync")})

	err := s.WriteVerdict(canonicalVerdict())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "verdict.json.tmp")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale verdict.json.tmp left on disk after Sync failure")
	}
}

func TestWriteVerdict_CloseError(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	withFlakyOpen(t, &flakyFile{failClose: errors.New("boom-close")})

	err := s.WriteVerdict(canonicalVerdict())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "verdict.json.tmp")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale verdict.json.tmp left on disk after Close failure")
	}
}

func TestWriteVerdict_RenameError(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "x", Path: dir}
	origRename := renameFn
	t.Cleanup(func() { renameFn = origRename })
	renameFn = func(oldPath, newPath string) error { return errors.New("boom-rename") }

	err := s.WriteVerdict(canonicalVerdict())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "verdict.json.tmp")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale verdict.json.tmp left on disk after Rename failure")
	}
}

// TestVerdictV1_RoundTrip ensures the schema unmarshals back into an
// equivalent struct. Catches breakage if a field tag drifts.
func TestVerdictV1_RoundTrip(t *testing.T) {
	v := canonicalVerdict()
	b, err := marshalVerdict(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got VerdictV1
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*v, got) {
		t.Fatalf("round trip drift\n--- got: %+v\n--- want: %+v", got, *v)
	}
}
