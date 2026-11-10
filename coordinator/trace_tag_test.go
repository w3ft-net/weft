package coordinator

import (
	"context"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// TestExecuteQuery_TraceTagsInteractiveOutput: a bare, interactively-
// typed `trace --id <id>` sets TraceTag on the request the coordinator
// hands to the compute pool, so compute/engine.go's display branch
// (summary line + colored [namespace/service] badges) engages.
func TestExecuteQuery_TraceTagsInteractiveOutput(t *testing.T) {
	c := setupTestCoordinator(t)
	pool := &spyCompute{inner: c.Compute}
	c.Compute = pool

	res, err := c.Execute(context.Background(), `trace --id abc-123`, fullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	drainExec(t, res)

	if pool.lastReq == nil {
		t.Fatal("ExecuteQuery was never called")
	}
	if !pool.lastReq.TraceTag {
		t.Error("interactive `trace --id abc-123` should set TraceTag=true, got false")
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

	pipeline := `trace --id abc-123`
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
		t.Error("watch-fired `trace --id abc-123` must not set TraceTag — would corrupt AlertCommand/dashboard consumers")
	}
}

// The span queries return JSON rows, so none of them is tagged: the summary
// line and badges are for a bare `trace <id>` only.
func TestExecuteQuery_SpanQueriesAreAggregatesNotTagged(t *testing.T) {
	for _, pipeline := range []string{"trace --spans", "trace --spans --name shard.Query", "trace --spans --id abc-123", "trace"} {
		t.Run(pipeline, func(t *testing.T) {
			c := setupTestCoordinator(t)
			pool := &spyCompute{inner: c.Compute}
			c.Compute = pool

			res, err := c.Execute(context.Background(), pipeline, fullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			drainExec(t, res)

			if pool.lastReq == nil {
				t.Fatal("ExecuteQuery was never called")
			}
			if pool.lastReq.TraceTag {
				t.Errorf("%q must not set TraceTag", pipeline)
			}
			if pool.lastReq.Aggregate == "" {
				t.Errorf("%q should run as a shard-side aggregate", pipeline)
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

	pipeline := `trace --spans`
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
		t.Error("watch-fired `trace --spans` must not set TraceTag — would corrupt AlertCommand/dashboard consumers")
	}
}
