package coordinator

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// spyCompute wraps a real compute.Executor and records every
// ExecuteQuery call so tests can assert that all queries route through it.
type spyCompute struct {
	execCount atomic.Int32
	lastReq   *pb.ExecuteQueryRequest
	inner     compute.Executor
}

func (p *spyCompute) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	p.execCount.Add(1)
	p.lastReq = req
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
