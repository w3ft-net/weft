//go:build !defaultshape

package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/shape/shapetest"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"

	"time"

	"github.com/graybeardcorp/weft/planner"
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

// TestExecute_PlainBashPipelineRunsLocally pins the unified-dispatch
// shape: Coordinator.Execute accepts any user-typed pipeline (log or
// not). For a plain non-log shell invocation it boots warp locally
// and returns whatever stdout the pipeline produces.
func TestExecute_PlainBashPipelineRunsLocally(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute(context.Background(), "echo 42", fullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resD := drainExec(t, res)
	if got := strings.TrimRight(resD.Text, "\n"); got != "42" {
		t.Errorf("Execute(\"echo 42\") text = %q, want \"42\"", got)
	}
}
