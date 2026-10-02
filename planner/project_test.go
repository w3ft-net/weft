package planner

import (
	"strings"
	"testing"
	"time"
)

// TestRawExpr_Cut verifies that cut projections produce the correct SQL raw expression.
func TestRawExpr_Cut(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantInSQL string
		noCompute bool
	}{
		{
			name:      "single field",
			pipeline:  `cat vimgym.log | cut -f3`,
			wantInSQL: "list_extract(fields, 3) as raw",
			noCompute: true,
		},
		{
			name:      "multiple fields",
			pipeline:  `cat vimgym.log | cut -f1,3`,
			wantInSQL: "concat_ws(' ', list_extract(fields, 1), list_extract(fields, 3)) as raw",
			noCompute: true,
		},
		{
			name:      "open-ended range",
			pipeline:  `cat vimgym.log | cut -f5-`,
			wantInSQL: "array_to_string(fields[5:], ' ') as raw",
			noCompute: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
				t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
			}
			if tt.noCompute && len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// TestRawExpr_AwkPrint verifies awk print projections.
func TestRawExpr_AwkPrint(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantInSQL string
		noCompute bool
	}{
		{
			name:      "print two fields",
			pipeline:  `cat vimgym.log | awk '{ print $2, $3 }'`,
			wantInSQL: "concat_ws(' ', list_extract(fields, 2), list_extract(fields, 3)) as raw",
			noCompute: true,
		},
		{
			name:      "print $0 (identity)",
			pipeline:  `cat vimgym.log | awk '{ print }'`,
			wantInSQL: "SELECT raw, ts FROM logs",
			noCompute: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
				t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
			}
			if tt.noCompute && len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// TestRawExpr_Jq verifies jq single-field projections collapse into a
// json_extract on raw and drop the jq compute stage.
func TestRawExpr_Jq(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantInSQL string
		wantComp  []string
	}{
		{
			name:      "jq field access",
			pipeline:  `cat vimgym.log | jq '.status'`,
			wantInSQL: "CAST(json_extract(payload, '$.status') AS VARCHAR) as raw",
			wantComp:  nil,
		},
		{
			name:      "jq select then field",
			pipeline:  `cat vimgym.log | jq 'select(.status == 500) | .message'`,
			wantInSQL: "CAST(json_extract(payload, '$.message') AS VARCHAR) as raw",
			wantComp:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
				t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
			}
			if len(plan.ComputeStages) != len(tt.wantComp) {
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v", len(plan.ComputeStages), len(tt.wantComp), plan.ComputeStages)
			}
			for i, want := range tt.wantComp {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]: got %q, want %q", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// TestRawExpr_NoProjection verifies that pipelines without projections
// use plain "raw" in the SELECT.
func TestRawExpr_NoProjection(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
	}{
		{"plain cat", `cat vimgym.log`},
		{"cat with grep", `cat vimgym.log | grep timeout`},
		{"cat with sort (compute)", `cat vimgym.log | sort`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.HasPrefix(plan.ShardSQL, "SELECT raw, ts FROM logs") {
				t.Errorf("expected plain raw SELECT, got: %s", plan.ShardSQL)
			}
		})
	}
}
