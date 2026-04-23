package planner

import (
	"reflect"
	"testing"
)

func TestSplitAlertStage(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantQP   string
		wantArgs []string
	}{
		{
			name:     "no alert stage",
			pipeline: `tail vimgym.log | grep ERROR`,
			wantQP:   `tail vimgym.log | grep ERROR`,
			wantArgs: nil,
		},
		{
			name:     "alert at end with condition",
			pipeline: `tail shard_metrics.log | jq -r '.pct' | alert above 5`,
			wantQP:   `tail shard_metrics.log | jq -r '.pct'`,
			wantArgs: []string{"above", "5"},
		},
		{
			name:     "alert with notify",
			pipeline: `cat foo.log | alert above 50 --notify=ops`,
			wantQP:   `cat foo.log`,
			wantArgs: []string{"above", "50", "--notify=ops"},
		},
		{
			name:     "alert with multi-row flags",
			pipeline: `cat foo.log | jq -r '"\(.host) \(.pct)"' | alert above 80 -k 2 -t 1 --notify=ops`,
			wantQP:   `cat foo.log | jq -r '"\(.host) \(.pct)"'`,
			wantArgs: []string{"above", "80", "-k", "2", "-t", "1", "--notify=ops"},
		},
		{
			name:     "alert only (no query)",
			pipeline: `alert above 5`,
			wantQP:   ``,
			wantArgs: []string{"above", "5"},
		},
		{
			name:     "alert mid-pipeline is not terminal",
			pipeline: `echo 1 | alert above 0 | cat`,
			wantQP:   `echo 1 | alert above 0 | cat`,
			wantArgs: nil,
		},
		{
			name:     "pipe character inside quoted jq filter",
			pipeline: `tail vimgym.log | jq -r 'select(.msg == "a | b")' | alert above 1`,
			wantQP:   `tail vimgym.log | jq -r 'select(.msg == "a | b")'`,
			wantArgs: []string{"above", "1"},
		},
		{
			name:     "alert with quoted --msg",
			pipeline: `cat foo.log | alert above 5 --msg "disk is too full"`,
			wantQP:   `cat foo.log`,
			wantArgs: []string{"above", "5", "--msg", "disk is too full"},
		},
		{
			name:     "alert as default condition (no keyword)",
			pipeline: `cat foo.log | grep ERROR | alert --notify=ops`,
			wantQP:   `cat foo.log | grep ERROR`,
			wantArgs: []string{"--notify=ops"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qp, args := SplitAlertStage(tt.pipeline)
			if qp != tt.wantQP {
				t.Errorf("queryPart = %q, want %q", qp, tt.wantQP)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("alertArgs = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}

func TestPlan_AlertArgsPropagated(t *testing.T) {
	// Source-backed pipeline: planner should treat alert as terminal,
	// not as a compute stage.
	plan, err := Plan(`tail shard_metrics.log | jq -r '.pct' | alert above 5`)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !reflect.DeepEqual(plan.AlertArgs, []string{"above", "5"}) {
		t.Errorf("AlertArgs = %v, want [above 5]", plan.AlertArgs)
	}
	for _, stage := range plan.ComputeStages {
		if stage == "alert above 5" || stage == "alert" {
			t.Errorf("alert leaked into ComputeStages: %v", plan.ComputeStages)
		}
	}

	// Local pipeline (`echo` is not a weft source): planner falls back to
	// Local mode but still extracts AlertArgs.
	plan, err = Plan(`echo 42 | alert above 0`)
	if err != nil {
		t.Fatalf("Plan local: %v", err)
	}
	if !plan.Local {
		t.Error("expected Local = true for echo pipeline")
	}
	if plan.LocalPipeline != "echo 42" {
		t.Errorf("LocalPipeline = %q, want %q", plan.LocalPipeline, "echo 42")
	}
	if !reflect.DeepEqual(plan.AlertArgs, []string{"above", "0"}) {
		t.Errorf("AlertArgs = %v, want [above 0]", plan.AlertArgs)
	}
}
