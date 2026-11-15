package coordinator

import (
	"errors"
	"testing"

	"github.com/w3ft-net/weft/shape/shapetest"

	"time"

	"github.com/w3ft-net/weft/planner"
)

// TestPlan_StrictRejectsNonLogPipeline drives the planner-side
// strictness step: planner.Plan returns ErrNotPlannable for any
// pipeline whose first command isn't a /log/... source. The
// coordinator's Execute path catches the error and routes through
// warp; Query propagates it.
func TestPlan_StrictRejectsNonLogPipeline(t *testing.T) {
	cases := []string{
		"echo 42",
		"echo 42 | alert above 0",
		"cat /tmp/notes",
		"seq 1 5 | wc -l",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			_, err := planner.Plan(p, shapetest.Tenant(1), "", time.Time{}, time.Time{})
			if !errors.Is(err, planner.ErrNotPlannable) {
				t.Fatalf("Plan(%q) err = %v, want ErrNotPlannable", p, err)
			}
		})
	}
}
