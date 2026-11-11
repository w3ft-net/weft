package planner

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logtable"
)

// A query that reads one (customer, namespace, service, kind) names the table
// that holds it, so a shard with a table per key reads only that table.
// docs/plans/per-service-tables.md.

func planTable(t *testing.T, customer int64, pipeline string) string {
	t.Helper()
	plan, err := Plan(pipeline, customer, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan(%q): %v", pipeline, err)
	}
	return plan.Table
}

func TestPlanTable_OneSourceNamesItsTable(t *testing.T) {
	want := logtable.Name([]any{int64(2)}, "vimgym", "api", "main")
	for _, pipeline := range []string{
		"cat /log/vimgym/api/main.log",
		"tail /log/vimgym/api/main.log",
		"head -n 5 /log/vimgym/api/main.log",
		"cat /log/vimgym/api/main.log | grep error | wc -l",
		// the env is a column, not part of the key
		"cat /log/vimgym/prod/api/main.log",
	} {
		if got := planTable(t, 2, pipeline); got != want {
			t.Errorf("%q: Table = %q, want %q", pipeline, got, want)
		}
	}
}

func TestPlanTable_TheCustomerIsPartOfTheName(t *testing.T) {
	a := planTable(t, 2, "cat /log/vimgym/api/main.log")
	b := planTable(t, 3, "cat /log/vimgym/api/main.log")
	if a == "" || a == b {
		t.Errorf("two customers reading the same path got tables %q and %q", a, b)
	}
}

// A cron runs the lowered form of a plan, so the table must survive lowering.
func TestLower_KeepsTheTable(t *testing.T) {
	lp, err := Lower("cat /log/vimgym/api/main.log | grep error", 2, "bolt", noResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if want := logtable.Name([]any{int64(2)}, "vimgym", "api", "main"); lp.Table != want {
		t.Errorf("LoweredPlan.Table = %q, want %q", lp.Table, want)
	}
}

// Anything that does not pin exactly one key leaves the table empty: the shard
// then has no hint, and the predicates alone pick the rows.
func TestPlanTable_EmptyUnlessOneKeyIsPinned(t *testing.T) {
	for _, c := range []struct {
		name     string
		customer int64
		pipeline string
	}{
		{"two services", 2, "cat /log/vimgym/api/main.log /log/vimgym/web/main.log"},
		{"two kinds", 2, "cat /log/vimgym/api/main.log /log/vimgym/api/heartbeats.log"},
		{"two namespaces", 2, "cat /log/vimgym/api/main.log /log/acme/api/main.log"},
		{"no customer", 0, "cat /log/vimgym/api/main.log"},
	} {
		if got := planTable(t, c.customer, c.pipeline); got != "" {
			t.Errorf("%s: Table = %q, want empty", c.name, got)
		}
	}
}
