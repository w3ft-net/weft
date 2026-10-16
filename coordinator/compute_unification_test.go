package coordinator

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
	weftsdk "github.com/w3ft-net/weft-go"
)

// spyCompute wraps a real compute.Executor and records every
// ExecuteQuery call so tests can assert that all queries route through it.
type spyCompute struct {
	execCount atomic.Int32
	lastReq   *pb.ExecuteQueryRequest
	lastCtx   context.Context
	inner     compute.Executor
}

func (p *spyCompute) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	p.execCount.Add(1)
	p.lastReq = req
	p.lastCtx = ctx
	return p.inner.ExecuteQuery(ctx, req)
}

// TestQuery_NoStages_RoutesThroughCompute pins the behavior that
// every query — including zero-stage shard-only queries — routes
// through Compute.ExecuteQuery.
func TestQuery_NoStages_RoutesThroughCompute(t *testing.T) {
	c := setupTestCoordinator(t)
	// setupTestCoordinator already registered InProcessResolvers on
	// c.Compute (a compute.Engine). Wrap it in spyCompute so we
	// can observe ExecuteQuery calls without breaking the resolution.
	pool := &spyCompute{inner: c.Compute}
	c.Compute = pool

	res, err := c.Execute(context.Background(), `cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) == 0 && resD.Text == "" {
		t.Fatalf("query produced no output")
	}

	if pool.execCount.Load() != 1 {
		t.Fatalf("expected exactly one ExecuteQuery call, got %d", pool.execCount.Load())
	}
}

// The trace ID on the ctx given to Execute must reach the compute client
// call (which couriers it over gRPC), on both the interactive path and the
// watch path.
func TestExecute_TraceIDReachesComputeCall(t *testing.T) {
	c := setupTestCoordinator(t)
	spy := &spyCompute{inner: c.Compute}
	c.Compute = spy

	ctx := weftsdk.ContextWithTraceID(context.Background(), "trace-coord-1")
	res, err := c.Execute(ctx, `cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	drainExec(t, res)
	if got, _ := weftsdk.TraceIDFromContext(spy.lastCtx); got != "trace-coord-1" {
		t.Fatalf("Execute: compute call saw trace %q, want trace-coord-1", got)
	}

	cust, err := c.Store.GetOrgByName("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	spy.lastCtx = nil
	ctx = weftsdk.ContextWithTraceID(context.Background(), "trace-coord-2")
	w := &store.CronWatch{OrgID: cust.ID, Name: "trace-watch", Schedule: "every 1m", Pipeline: `cat /log/vimgym.log`, Enabled: true}
	res, err = c.ExecuteForWatch(ctx, w.Pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	drainExec(t, res)
	if got, _ := weftsdk.TraceIDFromContext(spy.lastCtx); got != "trace-coord-2" {
		t.Fatalf("ExecuteForWatch: compute call saw trace %q, want trace-coord-2", got)
	}
}
