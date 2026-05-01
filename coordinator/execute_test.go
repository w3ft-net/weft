package coordinator

import (
	"errors"
	"strings"
	"testing"

	"github.com/kevlarcon/weft/planner"
)

// TestQuery_StrictRejectsNonLogPipeline pins Phase 3's planner-strictness
// change: Coordinator.Query is for plannable log queries only. A
// non-log pipeline like `echo 42` returns ErrNotPlannable instead of
// silently routing through the runLocal fallback. Callers that may
// pass arbitrary user input use Execute instead.
func TestQuery_StrictRejectsNonLogPipeline(t *testing.T) {
	c := setupTestCoordinator(t)

	_, err := c.Query("echo 42", fullRange, "kevlar", "")
	if !errors.Is(err, planner.ErrNotPlannable) {
		t.Fatalf("expected ErrNotPlannable for non-log pipeline, got %v", err)
	}
}

// TestExecute_PlainBashPipelineRunsLocally pins the Phase 3 split:
// Coordinator.Execute is the bash entry point, accepting any
// user-typed pipeline (log or not). For a plain non-log shell
// invocation it boots warp locally and returns whatever stdout the
// pipeline produces.
//
// After Phase 3, the gRPC server's user-facing entry routes to
// Execute, not Query. Query becomes strict (log-only); calling
// Query with "echo 42" returns ErrNotPlannable.
func TestExecute_PlainBashPipelineRunsLocally(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute("echo 42", fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimRight(res.Text, "\n"); got != "42" {
		t.Errorf("Execute(\"echo 42\") text = %q, want \"42\"", got)
	}
}
