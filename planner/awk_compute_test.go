package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// When awk's pattern is pushed into SQL as a prefilter only, the stage
// still runs in compute exactly as written, whatever its action contains.
func TestAwkPushdown_ComputeRunsTheProgramVerbatim(t *testing.T) {
	plan, err := planBare(`cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" { n++ ; print $1 $2 }'`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.ShardSQL, "level = 'ERROR'") {
		t.Errorf("expected the LEVEL comparison as a prefilter; SQL = %s", plan.ShardSQL)
	}
	want := `awk 'LEVEL == "ERROR" { n++ ; print $1 $2 }'`
	if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != want {
		t.Errorf("compute = %q, want [%q]", plan.ComputeStages, want)
	}
}

// Pushing one rule's pattern down filters the rows every other rule
// sees, so a multi-rule program pushes the OR of its patterns and runs
// whole in compute.
func TestAwkPushdown_SecondRuleKeepsItsRows(t *testing.T) {
	plan, err := planBare(`cat /log/acme/api/main.log | awk 'LEVEL == "a" { print "A" } LEVEL == "b" { print "B" }'`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.ShardSQL, "(level = 'a' OR level = 'b')") {
		t.Errorf("expected an OR prefilter over both rules; SQL = %s", plan.ShardSQL)
	}
	if len(plan.ComputeStages) != 1 || !strings.Contains(plan.ComputeStages[0], `print "B"`) {
		t.Errorf("compute = %q: rows matching the second rule are dropped", plan.ComputeStages)
	}
}

// A compute stage must re-parse to the argv the user typed: an awk
// program containing a single quote (close, escaped quote, reopen in the
// shell) can't be wrapped in bare single quotes.
func TestPlan_ComputeStageKeepsEmbeddedSingleQuote(t *testing.T) {
	plan, err := planBare(`cat /log/acme/api/main.log | sort | awk '{ print "it'\''s" }'`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	last := plan.ComputeStages[len(plan.ComputeStages)-1]
	script, err := bash.Parse(last)
	if err != nil {
		t.Fatalf("compute stage %q does not re-parse: %v", last, err)
	}
	words := script.Commands[0].First.Commands[0].(*bash.SimpleCommand).Words
	if got := words[1].Literal(); got != `{ print "it's" }` {
		t.Errorf("compute stage %q re-parses to program %q", last, got)
	}
}
