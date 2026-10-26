package planner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/shard"
)

// The cold-shard Lambda skips archived files by recognizing the time-bound
// predicates the planner emits (shard.PruneColdPartitions). If the planner's
// wording drifts, pruning would silently stop and every archived read would go
// back to opening every file: this fails first.
func TestPlanTimeBounds_ColdPartitionsArePruned(t *testing.T) {
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	plan, err := planner.Plan(`cat foo.log`, 1, "", start, end)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	args := make([]string, len(plan.Args))
	for i, a := range plan.Args {
		args[i] = a.(string)
	}
	pruned := shard.PruneColdPartitions(plan.DirectShardSQL, args)
	for _, want := range []string{"_part > '2026-10-13 12:00:00'", "_part <= '2026-10-14 12:05:00'"} {
		if !strings.Contains(pruned, want) {
			t.Errorf("planned SQL lost %q after pruning:\n%s", want, pruned)
		}
	}
}
