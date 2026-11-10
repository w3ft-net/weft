package coordinator

import (
	"context"
	"fmt"
	"github.com/graybeardcorp/weft/store"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// requestLog records every request the coordinator sends to compute.
type requestLog struct {
	inner compute.Executor
	mu    sync.Mutex
	reqs  []*pb.ExecuteQueryRequest
}

func (r *requestLog) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return r.inner.ExecuteQuery(ctx, req)
}

// keyedCoordinator is two shards of `key value` lines for vimgym/api/main, the
// second shard's values given by values1, and the facts each has told the
// coordinator (nil for a shard that has told it nothing).
func keyedCoordinator(t *testing.T, values0, values1 []string, facts0, facts1 []string) (*Coordinator, *requestLog) {
	t.Helper()
	c, log, _ := keyedCoordinatorShards(t, values0, values1, facts0, facts1)
	return c, log
}

// keyedCoordinatorShards is keyedCoordinator that also returns the shards, for a
// test that breaks one.
func keyedCoordinatorShards(t *testing.T, values0, values1 []string, facts0, facts1 []string) (*Coordinator, *requestLog, []*shard.DuckDBShard) {
	t.Helper()
	var shards []*shard.DuckDBShard
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: testQueryStore(t, "kevlar", "vimgym")})
	for i, spec := range []struct {
		values []string
		facts  []string
		start  time.Time
	}{{values0, facts0, t0}, {values1, facts1, t1}} {
		sh, err := shard.NewDuckDBShard(fmt.Sprintf("keyed-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sh.Close() })
		shards = append(shards, sh)
		var rows []*pb.LogRow
		for j, v := range spec.values {
			rows = append(rows, &pb.LogRow{
				Raw: fmt.Sprintf("%s %s", []string{"a", "b"}[j%2], v), TsUnixNs: spec.start.Add(time.Duration(j+1) * time.Second).UnixNano(),
				CustomerId: 1, Namespace: "vimgym", Service: "api", Kind: "main",
			})
		}
		if err := sh.Ingest(context.Background(), rows, nil); err != nil {
			t.Fatal(err)
		}
		addInProcessShard(t, c, &logdb.Shard{
			ID: sh.ID(), WindowStart: spec.start, WindowEnd: spec.start.Add(time.Hour),
			State: logdb.StateIngesting, Live: sh, CreatedAt: time.Now(),
		}, "vimgym/api/main")
		var deltas []*pb.FieldFactDelta
		for n, loc := range spec.facts {
			deltas = append(deltas, typedFact(1, "vimgym", "api", "main", "", loc, uint64(n+1)))
		}
		c.Provisioner.FoldFieldFacts(sh.ID(), deltas)
	}
	log := &requestLog{inner: c.Compute}
	c.Compute = log
	return c, log, shards
}

const keyedSumQuery = `cat /log/vimgym/api/main.log | awk '{ s[$1] += $2 } END { for (k in s) print k, s[k] }'`

func runKeyed(t *testing.T, c *Coordinator, query string) string {
	t.Helper()
	res, err := c.Execute(context.Background(), query, fullRange, store.TestOrgID("kevlar"), "")
	if err != nil {
		t.Fatal(err)
	}
	return drainExec(t, res).Text
}

// With the facts on every shard the sum runs in the shards: one request, a
// group and no stage.
func TestKeyedAggregate_RunsOnTheShardsWhenEveryShardHasTheFacts(t *testing.T) {
	both := []string{"$2", "$2#int"}
	c, log := keyedCoordinator(t, []string{"1", "2", "3"}, []string{"10", "20", "30"}, both, both)
	if got, want := runKeyed(t, c, keyedSumQuery), "a 44\nb 22\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
	if len(log.reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(log.reqs))
	}
	if req := log.reqs[0]; req.Group.GetRender() != pb.GroupRender_GROUP_RENDER_KEY_SUM || len(req.Stages) != 0 {
		t.Errorf("request group %v stages %v, want the shard's GROUP BY and no stage", req.Group, req.Stages)
	}
}

// One shard without the facts keeps the stage in compute for the whole query: the
// plan is exact for the shards whose facts say so, and there is no other kind of
// answer to merge it with.
func TestKeyedAggregate_BasePlanWhenAShardLacksTheFacts(t *testing.T) {
	both := []string{"$2", "$2#int"}
	c, log := keyedCoordinator(t, []string{"1", "2", "3"}, []string{"10", "20", "30"}, both, nil)
	if got, want := runKeyed(t, c, keyedSumQuery), "a 44\nb 22\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
	if len(log.reqs) != 1 || log.reqs[0].Group != nil || len(log.reqs[0].Stages) != 1 {
		t.Errorf("requests %v, want one with the stage and no group", log.reqs)
	}
}

// One failing shard fails the whole query: no partial answer from the others, for a
// plan that runs on the shards and for the one that runs in compute alike.
func TestKeyedAggregate_AFailingShardFailsTheQuery(t *testing.T) {
	both := []string{"$2", "$2#int"}
	for name, facts := range map[string][2][]string{"exact plan": {both, both}, "base plan": {both, nil}} {
		t.Run(name, func(t *testing.T) {
			c, _, shards := keyedCoordinatorShards(t, []string{"1", "2", "3"}, []string{"10", "20", "30"}, facts[0], facts[1])
			if err := shards[1].Close(); err != nil {
				t.Fatal(err)
			}
			res, err := c.Execute(context.Background(), keyedSumQuery, fullRange, store.TestOrgID("kevlar"), "")
			if err == nil {
				d, derr := res.Drain(context.Background())
				if derr == nil {
					t.Fatalf("a query over a failing shard returned %q", d.Text)
				}
			}
		})
	}
}
