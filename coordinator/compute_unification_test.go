package coordinator

import (
	"sync/atomic"
	"testing"

	"github.com/kevlarcon/weft/compute"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// spyPool wraps a real compute pool and records every ExecuteQuery
// call so tests can assert that all queries route through the pool.
type spyPool struct {
	execCount atomic.Int32
	inner     compute.ComputePool
}

func (p *spyPool) ExecuteQuery(req *pb.ExecuteQueryRequest) (string, error) {
	p.execCount.Add(1)
	return p.inner.ExecuteQuery(req)
}

func (p *spyPool) NewJob(stages []string, shardCount int, mergeKey string, limit int) compute.ComputeJob {
	return p.inner.NewJob(stages, shardCount, mergeKey, limit)
}

// TestQuery_NoStages_RoutesThroughComputePool pins the behavior that
// every query — including zero-stage shard-only queries — routes
// through Pool.ExecuteQuery.
func TestQuery_NoStages_RoutesThroughComputePool(t *testing.T) {
	c := setupTestCoordinator(t)
	pool := &spyPool{inner: &registryPool{registry: c.Registry}}
	c.Pool = pool

	res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
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

// TestNewCoordinator_DefaultsLocalPool pins the constructor invariant:
// every Coordinator has a Pool so queries never dispatch to nil.
func TestNewCoordinator_DefaultsLocalPool(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	if c.Pool == nil {
		t.Fatal("NewCoordinator returned a Coordinator with nil Pool; expected default LocalComputePool")
	}
}
