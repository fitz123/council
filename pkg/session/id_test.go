package session

import (
	"regexp"
	"sync"
	"testing"
	"time"
)

// idShape is the canonical regex from docs/plans/2026-04-20-v1-mvp.md Task 2.
// Format: <YYYY-MM-DDTHH-MM-SSZ>-<adverb>-<adjective>-<name>.
var idShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z-[a-z]+-[a-z]+-[a-z]+$`)

func TestNewID_Shape(t *testing.T) {
	now := time.Date(2026, 4, 19, 17, 2, 14, 0, time.UTC)
	id := NewID(now)
	if !idShape.MatchString(id) {
		t.Fatalf("id %q does not match %s", id, idShape)
	}
	const wantPrefix = "2026-04-19T17-02-14Z-"
	if id[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("id %q does not start with %q", id, wantPrefix)
	}
}

func TestNewID_TimezoneNormalizedToUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("LA tz not available: %v", err)
	}
	// 17:02:14 UTC == 09:02:14 PST on 2026-04-19, so a non-UTC clock
	// must still produce the UTC timestamp prefix.
	local := time.Date(2026, 4, 19, 9, 2, 14, 0, loc)
	id := NewID(local)
	const wantPrefix = "2026-04-19T16-02-14Z-" // PDT (DST) on this date
	if id[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("id %q does not start with %q", id, wantPrefix)
	}
}

func TestNewID_ConsecutiveCallsDiffer(t *testing.T) {
	now := time.Now().UTC()
	a := NewID(now)
	b := NewID(now)
	// Same input time, but petname suffix should diverge with overwhelming
	// probability (~10^9 combinations).
	if a == b {
		t.Fatalf("two consecutive NewID calls returned identical id %q", a)
	}
}

func TestNewID_ParallelStable(t *testing.T) {
	// Run NewID concurrently to assert no race in the underlying petname
	// generator (math/rand global is documented thread-safe in Go 1.20+,
	// but this test pins the contract). Also assert uniqueness: a
	// regression that seeded petname from a constant would produce
	// duplicates AND pass the shape check, so a shape-only assertion
	// would miss the bug.
	const n = 64
	now := time.Now().UTC()
	ids := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i] = NewID(now)
		}(i)
	}
	wg.Wait()
	seen := make(map[string]int, n)
	for i, id := range ids {
		if !idShape.MatchString(id) {
			t.Fatalf("ids[%d]=%q does not match shape regex", i, id)
		}
		if dup, exists := seen[id]; exists {
			t.Fatalf("duplicate id %q at indices %d and %d", id, dup, i)
		}
		seen[id] = i
	}
}
