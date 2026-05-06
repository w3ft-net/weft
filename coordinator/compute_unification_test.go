package coordinator

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kevlarcon/weft/compute"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// spyPool wraps a real LocalComputePool but records every NewJob call so
// tests can assert what stages the coordinator dispatches. It's the
// minimum apparatus to observe the Phase 2 behavior change: no-compute
// queries should reach the pool just like with-compute queries do.
type spyPool struct {
	mu      sync.Mutex
	calls   []spyJobCall
	created atomic.Int32
	inner   compute.ComputePool
}

type spyJobCall struct {
	Stages     []string
	ShardCount int
	MergeKey   string
	Limit      int
}

func (p *spyPool) ExecuteQuery(req *pb.ExecuteQueryRequest) (string, error) {
	return p.inner.ExecuteQuery(req)
}

func (p *spyPool) NewJob(stages []string, shardCount int, mergeKey string, limit int) compute.ComputeJob {
	p.mu.Lock()
	p.calls = append(p.calls, spyJobCall{
		Stages:     append([]string(nil), stages...),
		ShardCount: shardCount,
		MergeKey:   mergeKey,
		Limit:      limit,
	})
	p.mu.Unlock()
	p.created.Add(1)
	return p.inner.NewJob(stages, shardCount, mergeKey, limit)
}

// TestQuery_NoStages_RoutesThroughComputePool pins the Phase 2 behavior:
// a plain shard query (no compute stages) goes through the ComputePool
// just like a query with stages does. Today executeQuery short-circuits
// at coordinator.go:550, calling NewJob only when len(stages) > 0; the
// no-compute path collects rows via its own hand-rolled goroutine
// fan-in. After Phase 2 there's one path for both.
//
// The test also captures the planner-side expectation: the merge key
// the pool sees for a plain `cat vimgym.log` should be the same one it
// would see for `cat vimgym.log | wc -l` — both rely on the same
// ordering coming back from the shards. The exact mergeKey value is
// whatever classifyShardMode emits for that pipeline; the assertion
// here is just that NewJob was called.
func TestQuery_NoStages_RoutesThroughComputePool(t *testing.T) {
	c := setupTestCoordinator(t)
	pool := &spyPool{inner: &compute.LocalComputePool{}}
	c.Pool = pool

	res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	resD := drainExec(t, res)
	// Sanity — the query must return something so we know the pool
	// path actually ran end-to-end, not just that NewJob was called.
	if len(resD.Rows) == 0 && resD.Text == "" {
		t.Fatalf("query produced no output")
	}

	if pool.created.Load() != 1 {
		t.Fatalf("expected exactly one ComputeJob to be created for a no-compute query, got %d", pool.created.Load())
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.calls) != 1 {
		t.Fatalf("expected 1 NewJob call, got %d", len(pool.calls))
	}
	if len(pool.calls[0].Stages) != 0 {
		t.Errorf("expected empty stages for a no-compute query, got %v", pool.calls[0].Stages)
	}
}

// TestNewCoordinator_DefaultsLocalPool pins the constructor invariant
// that Phase 2 introduces: every Coordinator has a Pool. Today nil is
// allowed (and the no-compute path quietly handles it); after Phase 2
// every query routes through Pool, so a nil Pool would crash. Default-
// construct a LocalComputePool when CoordinatorConfig leaves Pool unset
// so callers don't need to wire one explicitly for the common case.
//
// Deliberately avoids testQueryStore — NewCoordinator does not touch
// Store during construction, so this assertion is independent of the
// Postgres dev DB being up.
func TestNewCoordinator_DefaultsLocalPool(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	if c.Pool == nil {
		t.Fatal("NewCoordinator returned a Coordinator with nil Pool; expected default LocalComputePool")
	}
}

// _ silences the unused-import warning in case future test edits drop
// the proto reference; harmless at zero cost. Remove when no longer
// needed.
var _ = (*pb.ResultRow)(nil)
