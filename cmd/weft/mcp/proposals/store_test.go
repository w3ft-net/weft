package proposals

import (
	"strings"
	"testing"
)

// TestStore_AddAssignsIdAndDiff: a fresh proposal gets a stable
// numeric ID (zero-padded so list ordering is lexicographic == time
// ordered) and a unified-diff text rendered from current/proposed.
// The diff is what the human sees in `weft propose review`, so it's
// the one field whose shape really matters here.
func TestStore_AddAssignsIdAndDiff(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id, err := s.Add(Proposal{
		Kind:      "script",
		Name:      "err-rate-by-window",
		Rationale: "extracted from 3 watches",
		Proposed:  "cat $1 | jq '.level' | alert above 50\n",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != "0001" {
		t.Errorf("first id = %q, want 0001", id)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "0001" {
		t.Errorf("ID = %q, want 0001", got.ID)
	}
	if !strings.Contains(got.Diff, "+cat $1 | jq '.level'") {
		t.Errorf("diff missing added line; got:\n%s", got.Diff)
	}
	if !strings.Contains(got.Diff, "bin/err-rate-by-window") {
		t.Errorf("diff missing path label; got:\n%s", got.Diff)
	}
}

// TestStore_AddIncrementsId: sequential adds get monotonically
// increasing IDs, padded so 0009 sorts before 0010 in any
// directory listing. This is what makes `weft propose review`
// show proposals in the order they were drafted.
func TestStore_AddIncrementsId(t *testing.T) {
	s, _ := New(t.TempDir())
	var ids []string
	for i := 0; i < 12; i++ {
		id, err := s.Add(Proposal{Kind: "script", Name: "x", Proposed: "y\n"})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		ids = append(ids, id)
	}
	if ids[0] != "0001" || ids[8] != "0009" || ids[9] != "0010" || ids[11] != "0012" {
		t.Errorf("unexpected id sequence: %v", ids)
	}
}

// TestStore_ListReturnsAllInOrder: List returns every staged
// proposal in ID order. That's the contract `weft propose review`
// relies on.
func TestStore_ListReturnsAllInOrder(t *testing.T) {
	s, _ := New(t.TempDir())
	want := []string{"a", "b", "c"}
	for _, n := range want {
		_, _ = s.Add(Proposal{Kind: "script", Name: n, Proposed: "body\n"})
	}
	ps, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ps) != len(want) {
		t.Fatalf("List len = %d, want %d", len(ps), len(want))
	}
	for i, p := range ps {
		if p.Name != want[i] {
			t.Errorf("List()[%d].Name = %q, want %q", i, p.Name, want[i])
		}
	}
}

// TestStore_RemoveDeletesFile: after Apply succeeds for a
// proposal, the store removes it so subsequent List doesn't show
// it. Sanity-checks that Remove is durable (the next List on a
// fresh Store sees the absence too).
func TestStore_RemoveDeletesFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	id, _ := s.Add(Proposal{Kind: "script", Name: "x", Proposed: "y\n"})
	if err := s.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if ps, _ := s.List(); len(ps) != 0 {
		t.Errorf("after Remove, List len = %d, want 0", len(ps))
	}
	// Re-open the same dir and confirm nothing leaks back.
	s2, _ := New(dir)
	if ps, _ := s2.List(); len(ps) != 0 {
		t.Errorf("after re-open, List len = %d, want 0", len(ps))
	}
}

// TestStore_GetMissing: asking for an unknown ID returns
// ErrNotFound, not a generic file-system error. Callers (and the
// MCP tools_apply layer) match on this sentinel.
func TestStore_GetMissing(t *testing.T) {
	s, _ := New(t.TempDir())
	_, err := s.Get("9999")
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	// We don't lock down the exact message but we lock down that
	// it is the documented sentinel.
	if !isNotFound(err) {
		t.Errorf("err = %v, want ErrNotFound-shaped", err)
	}
}

// TestStore_DeleteProposalShapesDiffAsRemoval: a delete-flavoured
// proposal must produce a diff whose proposed-side is /dev/null —
// that's how the human reading the diff knows the resource is
// going away rather than being modified.
func TestStore_DeleteProposalShapesDiffAsRemoval(t *testing.T) {
	s, _ := New(t.TempDir())
	id, err := s.Add(Proposal{
		Kind:    "delete",
		SubKind: "watch",
		Name:    "old-watch",
		Current: "alert above 50\n",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, _ := s.Get(id)
	if !strings.Contains(got.Diff, "/dev/null") {
		t.Errorf("delete diff should mention /dev/null; got:\n%s", got.Diff)
	}
	if !strings.Contains(got.Diff, "-alert above 50") {
		t.Errorf("delete diff should show removed line; got:\n%s", got.Diff)
	}
}
