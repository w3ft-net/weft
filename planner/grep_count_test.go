package planner

import (
	"strings"
	"testing"
	"time"
)

// grep -c counts the lines that match, so the shard splits each row into
// lines and filters those, and returns one row: a cold shard no longer
// streams every row back for compute to grep.
func TestGrepCountLowersToAShardCount(t *testing.T) {
	for _, tt := range []struct{ pipeline, wantPred string }{
		{`grep -c timeout`, `regexp_matches(raw, 'timeout')`},
		{`grep -c -i error`, `regexp_matches(raw, 'error', 'i')`},
		{`grep -ci error`, `regexp_matches(raw, 'error', 'i')`},
		{`grep -vc DEBUG`, `NOT regexp_matches(raw, 'DEBUG')`},
		{`grep -cE 'a|b'`, `regexp_matches(raw, 'a|b')`},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			plan, err := Plan(`cat vimgym.log | `+tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			wantSQL := `SELECT '' AS raw, min(ts) AS ts, count(*) AS count FROM (SELECT unnest(string_split(raw, chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?) AS t WHERE ` + tt.wantPred + ` HAVING count(*) > 0`
			if plan.ShardSQL != wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("compute stages = %v, want none: the shard returns the count", plan.ComputeStages)
			}
			if plan.Group == nil || plan.Group.Render != RenderTotal {
				t.Errorf("Group = %+v, want a total rendered in the shard", plan.Group)
			}
		})
	}
}

// Behind a line count the shard can't count: compute applies the count to
// the merged rows, and grep -c runs there, verbatim.
func TestGrepCountAfterTailStaysInCompute(t *testing.T) {
	plan, err := Plan(`tail -n 500 vimgym.log | grep -c timeout`, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Group != nil {
		t.Errorf("Group = %+v, want none: tail's line count is applied after the shards", plan.Group)
	}
	if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != "grep -c timeout" {
		t.Errorf("compute stages = %v, want grep -c timeout verbatim", plan.ComputeStages)
	}
	if strings.Contains(plan.ShardSQL, "regexp_matches") {
		t.Errorf("ShardSQL filters before tail's line count:\n%s", plan.ShardSQL)
	}
}

// Flags grep -c can't lower leave the stage in compute, verbatim.
func TestGrepCountKeepsUnloweredFlagsInCompute(t *testing.T) {
	plan, err := Plan(`cat vimgym.log | grep -cw timeout`, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != "grep -cw timeout" {
		t.Errorf("compute stages = %v, want grep -cw timeout verbatim", plan.ComputeStages)
	}
}
