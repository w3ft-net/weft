package planner

import (
	"strings"
	"testing"
	"time"
)

// TestJq_SelectNestedPathPushdown: a select on a nested path
// (.a.b, .a[0].b) lowers to shard SQL like a bare .field does, guarded
// so a step through the wrong type drops the row the way jq's error does
// (planner/difftest runs both sides on mixed-shape payloads).
func TestJq_SelectNestedPathPushdown(t *testing.T) {
	for _, pipeline := range []string{
		`cat vimgym.log | jq 'select(.disk.pct > 90)'`,
		`cat vimgym.log | jq 'select(.a.b.c == "x")'`,
		`cat vimgym.log | jq 'select(.items[0].n == 1)'`,
	} {
		t.Run(pipeline, func(t *testing.T) {
			plan, err := Plan(pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.ComputeStages) != 0 || !strings.Contains(plan.ShardSQL, "json_type(payload, '$") {
				t.Errorf("not pushed down\n sql: %s\n compute: %v", plan.ShardSQL, plan.ComputeStages)
			}
		})
	}
}

// TestJq_SubjectRelativeSelect: a select after a path step tests the
// value the program holds, and the stage emits that value, so the whole
// stage is a filter on the longer path plus a projection of the subject.
func TestJq_SubjectRelativeSelect(t *testing.T) {
	proj := func(path string) string { return jqProj("json_extract(payload, '" + path + "')") }
	for _, tt := range []struct {
		pipeline string
		where    string // a fragment the WHERE clause holds
		project  string // the projection
	}{
		{`jq '.foo | select(.bar == 1)'`, `json_extract(payload, '$.foo.bar')`, proj("$.foo")},
		{`jq '.foo | select(.bar == 1) | .bar'`, `json_extract(payload, '$.foo.bar')`, proj("$.foo.bar")},
		{`jq '.status | select(. > 100)'`, `json_extract(payload, '$.status')`, proj("$.status")},
		{`jq '.items | select(.[1] == "two") | .[2]'`, `json_extract_string(payload, '$.items[1]')`, proj("$.items[2]")},
		{`jq 'select($level == "ERROR") | .foo | select(.bar == 1)'`, `level = 'ERROR'`, proj("$.foo")},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			plan, err := Plan("cat vimgym.log | "+tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("compute = %v, want none", plan.ComputeStages)
			}
			if !strings.Contains(plan.ShardSQL, tt.where) || !strings.Contains(plan.ShardSQL, tt.project) {
				t.Errorf("want WHERE ... %s and %s\n got: %s", tt.where, tt.project, plan.ShardSQL)
			}
		})
	}
}

// TestJq_SubjectRelativeTail: a tail that isn't a path reads the payload
// from its root, so after a path step it can't lower. The selects before
// it still reach the shard as a prefilter, which the stage implies.
func TestJq_SubjectRelativeTail(t *testing.T) {
	stage := `jq '.foo | select(.bar == 1) | {x: .bar}'`
	plan, err := Plan("cat vimgym.log | "+stage, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != stage {
		t.Errorf("compute = %q, want [%q]", plan.ComputeStages, stage)
	}
	if !strings.Contains(plan.ShardSQL, `json_extract(payload, '$.foo.bar')`) {
		t.Errorf("prefilter missing: %s", plan.ShardSQL)
	}
}

// TestJq_SelectUnpushableNestedPaths: shapes that stay in compute. A
// negative index counts from the end in jq, which JSONPath can't say;
// iterating an array emits elements, not rows.
func TestJq_SelectUnpushableNestedPaths(t *testing.T) {
	for _, pipeline := range []string{
		`cat vimgym.log | jq 'select(.items[-1] == 1)'`,
		`cat vimgym.log | jq '.disk[] | select(.pct > 90)'`,
	} {
		t.Run(pipeline, func(t *testing.T) {
			plan, err := Plan(pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if strings.Contains(plan.ShardSQL, "json_type(payload, ") {
				t.Errorf("pushed down: %s", plan.ShardSQL)
			}
		})
	}
}
