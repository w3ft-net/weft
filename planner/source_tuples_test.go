package planner

import (
	"strings"
	"testing"
	"time"
)

// TestMultiSourceReadsTheNamedTuples pins gap 8 of docs/plans/tenant-scope.md:
// `cat a/api b/web` reads those two tuples, not {a,b} x {api,web}.
func TestMultiSourceReadsTheNamedTuples(t *testing.T) {
	plan, err := Plan(`cat /log/a/api/main.log /log/b/web/main.log`, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.ShardSQL, "namespace IN") {
		t.Errorf("sources a/api and b/web render as a per-column product, which also admits a/web and b/api:\n%s", plan.ShardSQL)
	}
	want := []any{"a", "api", "main", "b", "web", "main"}
	if len(plan.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", plan.Args, want)
	}
	for i := range want {
		if plan.Args[i] != want[i] {
			t.Errorf("Args = %v, want %v", plan.Args, want)
			break
		}
	}
}

// A set that is its own per-column product keeps the compact IN form.
func TestMultiSourceProductStaysCompact(t *testing.T) {
	plan, err := Plan(`cat /log/a/api/main.log /log/a/web/main.log /log/b/api/main.log /log/b/web/main.log`, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.ShardSQL, "namespace IN (?, ?) AND service IN (?, ?)") {
		t.Errorf("a full product should stay compact:\n%s", plan.ShardSQL)
	}
}

// warp.log beside a specific file reads every row, so the file cannot narrow it.
func TestWildcardSourceBesideFileIsNotNarrowed(t *testing.T) {
	plan, err := Plan(`cat /log/warp.log /log/a/api/main.log`, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.ShardSQL, "WHERE") {
		t.Errorf("warp.log names every row, but the SQL still narrows:\n%s", plan.ShardSQL)
	}
}
