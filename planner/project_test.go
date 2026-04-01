package planner

import (
	"reflect"
	"testing"
)

// ===========================================================================
// Projection tests
//
// A Project describes which fields/keys the pipeline actually needs from
// the shard. The planner extracts projections from cut, awk print, and jq
// object construction stages.
//
// When a projection is Complete, the stage is fully handled by the shard
// (no compute needed). When not Complete, the shard narrows its SELECT
// but the stage still runs in compute for formatting.
// ===========================================================================

// ---------------------------------------------------------------------------
// cut projections
// ---------------------------------------------------------------------------

func TestProject_Cut(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantProject *Project
		wantCompute []string // empty if projection is complete
	}{
		// cut with specific fields — complete projection
		{
			name:        "cut single field",
			pipeline:    `cat vimgym.log | cut -d' ' -f3`,
			wantProject: &Project{Fields: []int{3}, Complete: true},
			wantCompute: nil,
		},
		{
			name:        "cut multiple fields",
			pipeline:    `cat vimgym.log | cut -d' ' -f1,3,5`,
			wantProject: &Project{Fields: []int{1, 3, 5}, Complete: true},
			wantCompute: nil,
		},
		{
			name:        "cut field range",
			pipeline:    `cat vimgym.log | cut -d' ' -f2-4`,
			wantProject: &Project{Fields: []int{2, 3, 4}, Complete: true},
			wantCompute: nil,
		},
		{
			name:        "cut open-ended range",
			pipeline:    `cat vimgym.log | cut -d' ' -f5-`,
			wantProject: &Project{Fields: []int{-5}, Complete: true}, // -5 = open-ended from 5
			wantCompute: nil,
		},
		// cut with tab delimiter — still a projection but delimiter differs
		{
			name:        "cut with tab delimiter",
			pipeline:    `cat vimgym.log | cut -f3`,
			wantProject: &Project{Fields: []int{3}, Complete: true},
			wantCompute: nil,
		},
		// cut after a filter — filter pushes down, cut is a projection
		{
			name:        "filter then cut",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | cut -d' ' -f3,4`,
			wantProject: &Project{Fields: []int{3, 4}, Complete: true},
			wantCompute: nil,
		},
		// cut followed by more compute — projection narrows but compute remains
		{
			name:        "cut then sort",
			pipeline:    `cat vimgym.log | cut -d' ' -f3 | sort`,
			wantProject: &Project{Fields: []int{3}, Complete: false},
			wantCompute: []string{"sort"},
		},
		{
			name:        "cut then sort then uniq -c",
			pipeline:    `cat vimgym.log | cut -d' ' -f3 | sort | uniq -c`,
			wantProject: &Project{Fields: []int{3}, Complete: false},
			wantCompute: []string{"sort", "uniq -c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			assertProject(t, plan.Project, tt.wantProject)
			assertCompute(t, plan.ComputeStages, tt.wantCompute)
		})
	}
}

// ---------------------------------------------------------------------------
// awk print projections
// ---------------------------------------------------------------------------

func TestProject_AwkPrint(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantProject *Project
		wantCompute []string
	}{
		// awk print single field — fully pushed down as SQL
		{
			name:        "awk print single field",
			pipeline:    `cat vimgym.log | awk '{ print $3 }'`,
			wantProject: &Project{Fields: []int{3},
				RawExpr:  "list_extract(fields, 3)",
				Complete: true},
			wantCompute: nil,
		},
		// awk print multiple fields — comma-separated, pushed as concat_ws
		{
			name:        "awk print multiple fields",
			pipeline:    `cat vimgym.log | awk '{ print $1, $4 }'`,
			wantProject: &Project{Fields: []int{1, 4},
				RawExpr:  "concat_ws(' ', list_extract(fields, 1), list_extract(fields, 4))",
				Complete: true},
			wantCompute: nil,
		},
		// awk with filter + print — filter pushes down, print pushed as SQL
		{
			name:        "awk filter then awk print",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | awk '{ print $3, $5 }'`,
			wantProject: &Project{Fields: []int{3, 5},
				RawExpr:  "concat_ws(' ', list_extract(fields, 3), list_extract(fields, 5))",
				Complete: true},
			wantCompute: nil,
		},
		// awk partial pushdown: filter + print — both fully pushed to SQL
		{
			name:        "awk filter with print action",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { print $3, $4 }'`,
			wantProject: &Project{Fields: []int{3, 4},
				RawExpr:  "concat_ws(' ', list_extract(fields, 3), list_extract(fields, 4))",
				Complete: true},
			wantCompute: nil,
		},
		// awk print $0 — identity, complete (no compute needed)
		{
			name:        "awk print whole line",
			pipeline:    `cat vimgym.log | awk '{ print $0 }'`,
			wantProject: &Project{Complete: true},
			wantCompute: nil,
		},
		// awk print with string concatenation — pushed down as SQL concat
		{
			name:        "awk print with separator",
			pipeline:    `cat vimgym.log | awk '{ print $2 ":" $3 }'`,
			wantProject: &Project{Fields: []int{2, 3},
				RawExpr:  "concat(concat(list_extract(fields, 2), ':'), list_extract(fields, 3))",
				Complete: true},
			wantCompute: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			assertProject(t, plan.Project, tt.wantProject)
			assertCompute(t, plan.ComputeStages, tt.wantCompute)
		})
	}
}

// ---------------------------------------------------------------------------
// jq projections
// ---------------------------------------------------------------------------

func TestProject_Jq(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantProject *Project
		wantCompute []string
	}{
		// jq object construction — projects specific keys
		{
			name:        "jq object construction",
			pipeline:    `cat vimgym.log | jq '{user: .user_id, action: .action}'`,
			wantProject: &Project{JSONKeys: []string{"user_id", "action"}, Complete: false},
			wantCompute: []string{`jq '{user: .user_id, action: .action}'`},
		},
		// jq field access — projects a single key
		{
			name:        "jq single field",
			pipeline:    `cat vimgym.log | jq '.user_id'`,
			wantProject: &Project{JSONKeys: []string{"user_id"}, Complete: false},
			wantCompute: []string{`jq '.user_id'`},
		},
		// jq select then object — select pushes, construction is projection + compute
		{
			name:        "jq select then construction",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | {user: .user_id}'`,
			wantProject: &Project{JSONKeys: []string{"user_id"}, Complete: false},
			wantCompute: []string{`jq '{user: .user_id}'`},
		},
		// jq identity — no projection
		{
			name:        "jq identity",
			pipeline:    `cat vimgym.log | jq '.'`,
			wantProject: nil,
			wantCompute: []string{`jq '.'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			assertProject(t, plan.Project, tt.wantProject)
			assertCompute(t, plan.ComputeStages, tt.wantCompute)
		})
	}
}

// ---------------------------------------------------------------------------
// No projection cases
// ---------------------------------------------------------------------------

func TestProject_None(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
	}{
		{"bare cat", `cat vimgym.log`},
		{"filter only", `cat vimgym.log | awk '$LEVEL == "ERROR"'`},
		{"grep only", `cat vimgym.log | grep timeout`},
		{"sort only", `cat vimgym.log | sort`},
		{"wc only", `cat vimgym.log | wc -l`},
		{"head", `head -5 vimgym.log`},
		{"grep then sort", `cat vimgym.log | grep ERROR | sort`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.Project != nil {
				t.Errorf("expected no projection, got %+v", plan.Project)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mixed: projection + filter pushdown
// ---------------------------------------------------------------------------

func TestProject_WithFilter(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantProject *Project
		wantCompute []string
	}{
		{
			name:        "grep then cut",
			pipeline:    `cat vimgym.log | grep ERROR | cut -d' ' -f3`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%ERROR%'`,
			wantProject: &Project{Fields: []int{3}, Complete: true},
			wantCompute: nil,
		},
		{
			name:        "awk filter then jq construction",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | jq '{user: .user_id}'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR'`,
			wantProject: &Project{JSONKeys: []string{"user_id"}, Complete: false},
			wantCompute: []string{`jq '{user: .user_id}'`},
		},
		{
			name:        "jq select + field access",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .payload'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantProject: &Project{JSONKeys: []string{"payload"}, Complete: false},
			wantCompute: []string{`jq '.payload'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			assertProject(t, plan.Project, tt.wantProject)
			assertCompute(t, plan.ComputeStages, tt.wantCompute)
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertProject(t *testing.T, got, want *Project) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("Project: expected nil, got %+v", got)
		}
		return
	}
	if got == nil {
		t.Fatalf("Project: expected %+v, got nil", want)
	}
	if !reflect.DeepEqual(got.Fields, want.Fields) {
		t.Errorf("Project.Fields\n got: %v\nwant: %v", got.Fields, want.Fields)
	}
	if !reflect.DeepEqual(got.JSONKeys, want.JSONKeys) {
		t.Errorf("Project.JSONKeys\n got: %v\nwant: %v", got.JSONKeys, want.JSONKeys)
	}
	if got.RawExpr != want.RawExpr {
		t.Errorf("Project.RawExpr\n got: %s\nwant: %s", got.RawExpr, want.RawExpr)
	}
	if got.Complete != want.Complete {
		t.Errorf("Project.Complete: got %v, want %v", got.Complete, want.Complete)
	}
}

func assertCompute(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, got[i], w)
		}
	}
}
