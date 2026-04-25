package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// canonicalVerdict mirrors the v2 example embedded in
// testdata/verdict_canonical.json byte-for-byte after marshaling. Drift on
// any non-time/non-duration field breaks the schema contract and fails
// TestVerdict_BytesExact below.
//
// Shape covers every fitness-gated field: anonymization map, per-round
// per-expert {label, real_name, participation}, top-level experts with
// participation_by_round, voting winner (xor tied_candidates), ballots.
func canonicalVerdict() *Verdict {
	return &Verdict{
		Version:     2,
		SessionID:   "2026-04-22T10-14-30Z-fizzy-jingling-quokka",
		SessionPath: "./.council/sessions/2026-04-22T10-14-30Z-fizzy-jingling-quokka",
		Profile:     "default",
		Question:    "Should we use Raft or Paxos for our cluster?",
		Answer:      "Raft is the answer.",
		Status:      "ok",
		Anonymization: map[string]string{
			"A": "curious-albatross",
			"B": "devoted-bumblebee",
			"C": "eager-capybara",
		},
		Rounds: []Round{
			{Experts: []ExpertResult{
				{Label: "A", RealName: "curious-albatross", Participation: "ok", Executor: "claude-code", Model: "sonnet", ExitCode: 0, Retries: 0, DurationSeconds: 17.3},
				{Label: "B", RealName: "devoted-bumblebee", Participation: "ok", Executor: "claude-code", Model: "sonnet", ExitCode: 0, Retries: 0, DurationSeconds: 19.1},
				{Label: "C", RealName: "eager-capybara", Participation: "ok", Executor: "claude-code", Model: "sonnet", ExitCode: 0, Retries: 1, DurationSeconds: 22.4},
			}},
			{Experts: []ExpertResult{
				{Label: "A", RealName: "curious-albatross", Participation: "ok", Executor: "claude-code", Model: "sonnet", ExitCode: 0, Retries: 0, DurationSeconds: 15.2},
				{Label: "B", RealName: "devoted-bumblebee", Participation: "carried", Executor: "claude-code", Model: "sonnet", ExitCode: 0, Retries: 0, DurationSeconds: 0},
				{Label: "C", RealName: "eager-capybara", Participation: "ok", Executor: "claude-code", Model: "sonnet", ExitCode: 0, Retries: 0, DurationSeconds: 18.8},
			}},
		},
		Experts: []ExpertSummary{
			{Label: "A", RealName: "curious-albatross", ParticipationByRound: []string{"ok", "ok"}},
			{Label: "B", RealName: "devoted-bumblebee", ParticipationByRound: []string{"ok", "carried"}},
			{Label: "C", RealName: "eager-capybara", ParticipationByRound: []string{"ok", "ok"}},
		},
		Voting: &VerdictVoting{
			Votes:  map[string]int{"A": 2, "B": 0, "C": 1},
			Winner: "A",
			Ballots: []VerdictBallot{
				{VoterLabel: "A", VotedFor: "A"},
				{VoterLabel: "B", VotedFor: "A"},
				{VoterLabel: "C", VotedFor: "C"},
			},
		},
		StartedAt:       "2026-04-22T10:14:30Z",
		EndedAt:         "2026-04-22T10:15:28Z",
		DurationSeconds: 58.2,
	}
}

func TestVerdict_BytesExact(t *testing.T) {
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

// TestVerdict_V2_Shape enforces the shape invariants from Task 9 / the
// design doc §6:
//   - version == 2
//   - rounds[] present with per-round expert entries carrying
//     {label, real_name, participation}
//   - experts[] top-level summary present
//   - voting.votes object present
//   - exactly one of voting.winner / voting.tied_candidates populated
//   - anonymization map populated
//   - status is a v2-valid terminal value
func TestVerdict_V2_Shape(t *testing.T) {
	v := canonicalVerdict()
	if v.Version != 2 {
		t.Errorf("Version = %d, want 2", v.Version)
	}
	if len(v.Rounds) == 0 {
		t.Fatal("Rounds must be non-empty")
	}
	for i, r := range v.Rounds {
		if len(r.Experts) == 0 {
			t.Errorf("Rounds[%d].Experts empty", i)
		}
		for j, e := range r.Experts {
			if e.Label == "" {
				t.Errorf("Rounds[%d].Experts[%d].Label empty", i, j)
			}
			if e.RealName == "" {
				t.Errorf("Rounds[%d].Experts[%d].RealName empty", i, j)
			}
			if e.Participation == "" {
				t.Errorf("Rounds[%d].Experts[%d].Participation empty", i, j)
			}
		}
	}
	if len(v.Experts) == 0 {
		t.Fatal("Experts top-level summary empty")
	}
	if v.Voting == nil {
		t.Fatal("Voting nil")
	}
	if v.Voting.Votes == nil {
		t.Error("Voting.Votes nil")
	}
	if v.Anonymization == nil {
		t.Error("Anonymization nil")
	}
	switch v.Status {
	case "ok", "no_consensus", "quorum_failed_round_1", "quorum_failed_round_2",
		"injection_suspected_in_question", "config_error", "interrupted", "error":
	default:
		t.Errorf("Status = %q, not a v2-valid terminal value", v.Status)
	}
}

// TestVerdict_F7_ParticipationByRoundLength gates fitness F7
// (docs/design/v2.md §8 F7): every top-level expert summary's
// participation_by_round slice has length equal to len(rounds). The jq
// probe is `.experts[].participation_by_round | length == (.rounds |
// length)`. K-agnostic: the test runs over several {K, expert} shapes.
func TestVerdict_F7_ParticipationByRoundLength(t *testing.T) {
	cases := []struct {
		name string
		v    *Verdict
	}{
		{name: "canonical (K=2, N=3)", v: canonicalVerdict()},
		{
			name: "single round (K=1) with N=2 — defensive against future K=1 profile",
			v: &Verdict{
				Version: 2,
				Rounds: []Round{
					{Experts: []ExpertResult{
						{Label: "A", RealName: "x", Participation: "ok"},
						{Label: "B", RealName: "y", Participation: "failed"},
					}},
				},
				Experts: []ExpertSummary{
					{Label: "A", RealName: "x", ParticipationByRound: []string{"ok"}},
					{Label: "B", RealName: "y", ParticipationByRound: []string{"failed"}},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := len(tc.v.Rounds)
			for _, e := range tc.v.Experts {
				if got := len(e.ParticipationByRound); got != want {
					t.Errorf("expert %s: participation_by_round len = %d, want %d", e.Label, got, want)
				}
			}
		})
	}
}

// TestVerdict_F8_AnonymizationConsistency gates fitness F8
// (docs/design/v2.md §8 F8): every (label, real_name) pair observed under
// rounds[].experts[] resolves to the same real_name in the top-level
// anonymization map. The jq probe is `anonymization as $m | [.rounds[]
// .experts[] | {label, real_name}] | unique | all(. as $e | $m[$e.label]
// == $e.real_name)`. We also check the top-level experts[] summary since
// F7 requires consistent labels there.
func TestVerdict_F8_AnonymizationConsistency(t *testing.T) {
	v := canonicalVerdict()
	if len(v.Anonymization) == 0 {
		t.Fatal("canonical verdict missing anonymization map")
	}
	for i, r := range v.Rounds {
		for j, e := range r.Experts {
			got, ok := v.Anonymization[e.Label]
			if !ok {
				t.Errorf("rounds[%d].experts[%d] label %q not in anonymization", i, j, e.Label)
				continue
			}
			if got != e.RealName {
				t.Errorf("rounds[%d].experts[%d] label %q: real_name = %q, want %q",
					i, j, e.Label, e.RealName, got)
			}
		}
	}
	for i, e := range v.Experts {
		got, ok := v.Anonymization[e.Label]
		if !ok {
			t.Errorf("experts[%d] label %q not in anonymization", i, e.Label)
			continue
		}
		if got != e.RealName {
			t.Errorf("experts[%d] label %q: real_name = %q, want %q", i, e.Label, e.RealName, got)
		}
	}
}

// TestVerdict_F12_WinnerXorTied gates fitness F12 (docs/design/v2.md §8
// F12): exactly one of voting.winner and voting.tied_candidates is
// populated per run. `omitempty` on both struct fields produces a JSON
// object where at most one key appears; a valid v2 run requires that
// exactly one does.
func TestVerdict_F12_WinnerXorTied(t *testing.T) {
	cases := []struct {
		name    string
		voting  *VerdictVoting
		wantErr bool
	}{
		{
			name:   "winner only (canonical)",
			voting: &VerdictVoting{Votes: map[string]int{"A": 2, "B": 1}, Winner: "A"},
		},
		{
			name:   "tied only",
			voting: &VerdictVoting{Votes: map[string]int{"A": 1, "B": 1, "C": 1}, TiedCandidates: []string{"A", "B", "C"}},
		},
		{
			name:    "both populated — fitness violation",
			voting:  &VerdictVoting{Votes: map[string]int{"A": 1}, Winner: "A", TiedCandidates: []string{"A", "B"}},
			wantErr: true,
		},
		{
			name:    "neither populated — fitness violation",
			voting:  &VerdictVoting{Votes: map[string]int{"A": 0}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			winnerSet := tc.voting.Winner != ""
			tiedSet := len(tc.voting.TiedCandidates) > 0
			exactlyOne := winnerSet != tiedSet
			if exactlyOne == tc.wantErr {
				t.Errorf("winner=%q tied=%v: exactlyOne=%v, want wantErr=%v",
					tc.voting.Winner, tc.voting.TiedCandidates, exactlyOne, tc.wantErr)
			}

			b, err := json.Marshal(tc.voting)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var generic map[string]any
			if err := json.Unmarshal(b, &generic); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, hasWinnerKey := generic["winner"]
			_, hasTiedKey := generic["tied_candidates"]
			if !tc.wantErr {
				if hasWinnerKey == hasTiedKey {
					t.Errorf("JSON: hasWinnerKey=%v hasTiedKey=%v — want exactly one", hasWinnerKey, hasTiedKey)
				}
			}
		})
	}
}

// TestVerdict_RateLimitsOmittedWhenEmpty asserts the omitempty contract on
// the new RateLimits field: the canonical happy-path verdict must not grow a
// "rate_limits" JSON key. Without omitempty (or with `[]session.RateLimitEntry{}`
// instead of nil), the canonical bytes-exact fixture would drift on every
// run.
func TestVerdict_RateLimitsOmittedWhenEmpty(t *testing.T) {
	v := canonicalVerdict()
	if len(v.RateLimits) != 0 {
		t.Fatalf("canonical verdict already has RateLimits set; test premise broken")
	}
	buf, err := marshalVerdict(v)
	if err != nil {
		t.Fatalf("marshalVerdict: %v", err)
	}
	if strings.Contains(string(buf), "rate_limits") {
		t.Errorf("rate_limits key present in happy-path verdict bytes:\n%s", buf)
	}
}

// TestVerdict_RateLimitsRoundTrip pins the wire shape of a populated
// rate_limits[] entry: each field is rendered with the documented JSON tag
// and round-trips through encode/decode without loss. This is the unit-level
// counterpart to F26 (verdict.json carries rate_limits[] when a CLI hits its
// quota mid-debate).
func TestVerdict_RateLimitsRoundTrip(t *testing.T) {
	v := canonicalVerdict()
	v.RateLimits = []RateLimitEntry{
		{Executor: "codex", Pattern: "you've hit your usage limit", HelpCmd: "codex /status", Round: 1, Expert: "B"},
		{Executor: "gemini-cli", Pattern: "RESOURCE_EXHAUSTED", HelpCmd: "check https://aistudio.google.com/apikey for quota and billing", Round: 2, Expert: "C"},
	}
	buf, err := marshalVerdict(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(buf, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rls, ok := generic["rate_limits"].([]any)
	if !ok {
		t.Fatalf("rate_limits key missing or not an array; got %T", generic["rate_limits"])
	}
	if len(rls) != 2 {
		t.Fatalf("rate_limits length = %d, want 2", len(rls))
	}
	first := rls[0].(map[string]any)
	for _, key := range []string{"executor", "pattern", "help_cmd", "round", "expert"} {
		if _, ok := first[key]; !ok {
			t.Errorf("rate_limits[0] missing key %q: %v", key, first)
		}
	}
	if first["executor"] != "codex" {
		t.Errorf("rate_limits[0].executor = %v, want codex", first["executor"])
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
			var v Verdict
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

// TestVerdict_RoundTrip ensures the schema unmarshals back into an
// equivalent struct. Catches breakage if a field tag drifts.
func TestVerdict_RoundTrip(t *testing.T) {
	v := canonicalVerdict()
	b, err := marshalVerdict(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Verdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*v, got) {
		t.Fatalf("round trip drift\n--- got: %+v\n--- want: %+v", got, *v)
	}
}
