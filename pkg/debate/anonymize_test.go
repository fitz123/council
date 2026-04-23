package debate

import (
	"fmt"
	"testing"
)

func TestAssignLabels_Deterministic(t *testing.T) {
	experts := []Expert{{Name: "alpha"}, {Name: "bravo"}, {Name: "charlie"}}

	m1, err := AssignLabels("session-001", experts)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	m2, err := AssignLabels("session-001", experts)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(m1) != len(m2) {
		t.Fatalf("map sizes differ: %d vs %d", len(m1), len(m2))
	}
	for k, v := range m1 {
		if m2[k] != v {
			t.Fatalf("non-deterministic: label %q -> %q then %q", k, v, m2[k])
		}
	}
}

func TestAssignLabels_VariesAcrossSessions(t *testing.T) {
	experts := []Expert{
		{Name: "alpha"}, {Name: "bravo"}, {Name: "charlie"},
		{Name: "delta"}, {Name: "echo"},
	}
	var first string
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("session-%03d", i)
		m, err := AssignLabels(id, experts)
		if err != nil {
			t.Fatalf("AssignLabels(%q): %v", id, err)
		}
		key := m["A"] + "|" + m["B"] + "|" + m["C"] + "|" + m["D"] + "|" + m["E"]
		if i == 0 {
			first = key
			continue
		}
		if key != first {
			return
		}
	}
	t.Fatalf("50 distinct session IDs all produced the same mapping — shuffle is not session-dependent")
}

func TestAssignLabels_ShuffleHappens(t *testing.T) {
	// With N=10 experts, the identity mapping (A=e0, ..., J=e9) should not
	// persist across every session ID — at least one shuffle in 20 must
	// differ from identity or PCG seeding is broken.
	experts := make([]Expert, 10)
	for i := range experts {
		experts[i].Name = fmt.Sprintf("e%d", i)
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		m, err := AssignLabels(id, experts)
		if err != nil {
			t.Fatalf("AssignLabels: %v", err)
		}
		identity := true
		for idx, e := range experts {
			label := string(rune('A' + idx))
			if m[label] != e.Name {
				identity = false
				break
			}
		}
		if !identity {
			return
		}
	}
	t.Fatalf("all 20 sessions produced identity mapping — shuffle did not occur")
}

func TestAssignLabels_N3(t *testing.T) {
	experts := []Expert{{Name: "x"}, {Name: "y"}, {Name: "z"}}
	m, err := AssignLabels("s", experts)
	if err != nil {
		t.Fatalf("AssignLabels: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(m))
	}
	for _, label := range []string{"A", "B", "C"} {
		if _, ok := m[label]; !ok {
			t.Fatalf("missing label %q", label)
		}
		if len(label) != 1 {
			t.Fatalf("label %q is not single-letter", label)
		}
	}
	gotNames := map[string]bool{}
	for _, v := range m {
		gotNames[v] = true
	}
	for _, want := range []string{"x", "y", "z"} {
		if !gotNames[want] {
			t.Fatalf("real name %q missing from mapping", want)
		}
	}
}

func TestAssignLabels_AllExpertsPreserved(t *testing.T) {
	// No name should be dropped or duplicated regardless of shuffle.
	experts := make([]Expert, 8)
	for i := range experts {
		experts[i].Name = fmt.Sprintf("expert_%d", i)
	}
	m, err := AssignLabels("abc-123", experts)
	if err != nil {
		t.Fatalf("AssignLabels: %v", err)
	}
	if len(m) != len(experts) {
		t.Fatalf("len=%d, want %d", len(m), len(experts))
	}
	seen := map[string]int{}
	for _, v := range m {
		seen[v]++
	}
	for _, e := range experts {
		if seen[e.Name] != 1 {
			t.Fatalf("expert %q appears %d times (want 1)", e.Name, seen[e.Name])
		}
	}
}

func TestAssignLabels_AtLimit26(t *testing.T) {
	experts := make([]Expert, 26)
	for i := range experts {
		experts[i].Name = fmt.Sprintf("e%02d", i)
	}
	m, err := AssignLabels("s", experts)
	if err != nil {
		t.Fatalf("AssignLabels(26): %v", err)
	}
	if len(m) != 26 {
		t.Fatalf("expected 26 entries, got %d", len(m))
	}
	if _, ok := m["Z"]; !ok {
		t.Fatalf("expected label Z for N=26")
	}
}

func TestAssignLabels_ExceedsLimit(t *testing.T) {
	experts := make([]Expert, 27)
	for i := range experts {
		experts[i].Name = fmt.Sprintf("e%02d", i)
	}
	_, err := AssignLabels("s", experts)
	if err == nil {
		t.Fatalf("expected error for N=27, got nil")
	}
}

func TestAssignLabels_Empty(t *testing.T) {
	m, err := AssignLabels("s", nil)
	if err != nil {
		t.Fatalf("AssignLabels(nil): %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(m))
	}
}

func TestLabelOf_Found(t *testing.T) {
	m := map[string]string{"A": "alpha", "B": "bravo", "C": "charlie"}
	label, ok := LabelOf("bravo", m)
	if !ok {
		t.Fatalf("LabelOf(bravo): ok=false, want true")
	}
	if label != "B" {
		t.Fatalf("LabelOf(bravo) = %q, want B", label)
	}
}

func TestLabelOf_NotFound(t *testing.T) {
	m := map[string]string{"A": "alpha", "B": "bravo"}
	label, ok := LabelOf("nobody", m)
	if ok {
		t.Fatalf("LabelOf(nobody): ok=true, want false")
	}
	if label != "" {
		t.Fatalf("LabelOf(nobody) label = %q, want empty", label)
	}
}

func TestLabelOf_EmptyMap(t *testing.T) {
	_, ok := LabelOf("alpha", nil)
	if ok {
		t.Fatalf("LabelOf on nil map: ok=true, want false")
	}
}
