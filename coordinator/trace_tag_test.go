package coordinator

import (
	"context"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// TestExecuteQuery_TraceTagsInteractiveOutput: a bare, interactively-
// typed `trace <id>` sets TraceTag on the request the coordinator
// hands to the compute pool, so compute/engine.go's display branch
// (summary line + colored [namespace/service] badges) engages.
func TestExecuteQuery_TraceTagsInteractiveOutput(t *testing.T) {
	c := setupTestCoordinator(t)
	pool := &spyCompute{inner: c.Compute}
	c.Compute = pool

	res, err := c.Execute(context.Background(), `trace abc-123`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	drainExec(t, res)

	if pool.lastReq == nil {
		t.Fatal("ExecuteQuery was never called")
	}
	if !pool.lastReq.TraceTag {
		t.Error("interactive `trace abc-123` should set TraceTag=true, got false")
	}
}

// TestExecuteForWatch_TraceDoesNotTagOutput is the regression test for
// the bug this feature's design caught: `trace <id>` saved as a
// cron/watch and fired via ExecuteForWatch's cold-path fallback (no
// hot LoweredPlan) must NOT get TraceTag — a fabricated summary row
// and ANSI escape codes spliced into the output would corrupt
// AlertCommand's stdin-line evaluation and pollute dashboard tiles.
// The coordinator's alertCtx != nil gate (coordinator.go's
// executeQuery) is what prevents this.
func TestExecuteForWatch_TraceDoesNotTagOutput(t *testing.T) {
	c := setupTestCoordinator(t)
	pool := &spyCompute{inner: c.Compute}
	c.Compute = pool

	cust, err := c.Store.GetOrgByName("kevlar")
	if err != nil {
		t.Fatalf("resolve test customer: %v", err)
	}

	pipeline := `trace abc-123`
	w := &store.CronWatch{
		OrgID:    cust.ID,
		Name:     "trace-watch",
		Schedule: "every 1m",
		Pipeline: pipeline,
		Enabled:  true,
		// No LoweredPlan: forces the cold-path fallback through
		// executeQuery with alertCtx != nil, exercising the same gate
		// as a stale/never-lowered watch fire in production.
	}

	res, err := c.ExecuteForWatch(context.Background(), pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	drainExec(t, res)

	if pool.lastReq == nil {
		t.Fatal("ExecuteQuery was never called")
	}
	if pool.lastReq.TraceTag {
		t.Error("watch-fired `trace abc-123` must not set TraceTag — would corrupt AlertCommand/dashboard consumers")
	}
}

// TestExecuteQuery_TraceSpansTagsInteractiveOutput mirrors
// TestExecuteQuery_TraceTagsInteractiveOutput for Phase 2's cross-trace
// aggregation (docs/plans/span-display.md): `trace spans [name]` reuses
// the exact same buffer-then-emit path a bare `trace <id>` gets, so it
// needs the same TraceTag=true.
func TestExecuteQuery_TraceSpansTagsInteractiveOutput(t *testing.T) {
	for _, pipeline := range []string{"trace spans", "trace spans shard.Query"} {
		t.Run(pipeline, func(t *testing.T) {
			c := setupTestCoordinator(t)
			pool := &spyCompute{inner: c.Compute}
			c.Compute = pool

			res, err := c.Execute(context.Background(), pipeline, fullRange, "kevlar", "")
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			drainExec(t, res)

			if pool.lastReq == nil {
				t.Fatal("ExecuteQuery was never called")
			}
			if !pool.lastReq.TraceTag {
				t.Errorf("interactive %q should set TraceTag=true, got false", pipeline)
			}
		})
	}
}

// TestExecuteForWatch_TraceSpansDoesNotTagOutput is
// TestExecuteForWatch_TraceDoesNotTagOutput's Phase 2 counterpart —
// same corruption risk, same alertCtx != nil gate.
func TestExecuteForWatch_TraceSpansDoesNotTagOutput(t *testing.T) {
	c := setupTestCoordinator(t)
	pool := &spyCompute{inner: c.Compute}
	c.Compute = pool

	cust, err := c.Store.GetOrgByName("kevlar")
	if err != nil {
		t.Fatalf("resolve test customer: %v", err)
	}

	pipeline := `trace spans`
	w := &store.CronWatch{
		OrgID:    cust.ID,
		Name:     "trace-spans-watch",
		Schedule: "every 1m",
		Pipeline: pipeline,
		Enabled:  true,
	}

	res, err := c.ExecuteForWatch(context.Background(), pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	drainExec(t, res)

	if pool.lastReq == nil {
		t.Fatal("ExecuteQuery was never called")
	}
	if pool.lastReq.TraceTag {
		t.Error("watch-fired `trace spans` must not set TraceTag — would corrupt AlertCommand/dashboard consumers")
	}
}
