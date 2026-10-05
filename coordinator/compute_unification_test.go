package coordinator

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// spyPool wraps a real compute pool and records every ExecuteQuery
// call so tests can assert that all queries route through the pool.
type spyPool struct {
	execCount atomic.Int32
	lastReq   *pb.ExecuteQueryRequest
	inner     compute.ComputePool
}

func (p *spyPool) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	p.execCount.Add(1)
	p.lastReq = req
	return p.inner.ExecuteQuery(ctx, req)
}

func (p *spyPool) NewJob(stages []string, shardCount int, mergeKey string, limit int, keepLast bool, traceTag bool) compute.ComputeJob {
	return p.inner.NewJob(stages, shardCount, mergeKey, limit, keepLast, traceTag)
}

// TestQuery_NoStages_RoutesThroughComputePool pins the behavior that
// every query — including zero-stage shard-only queries — routes
// through Pool.ExecuteQuery.
func TestQuery_NoStages_RoutesThroughComputePool(t *testing.T) {
	c := setupTestCoordinator(t)
	// setupTestCoordinator already registered InProcessResolvers on
	// c.Pool (a LocalComputePool). Wrap it in spyPool so we
	// can observe ExecuteQuery calls without breaking the resolution.
	pool := &spyPool{inner: c.Pool}
	c.Pool = pool

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
