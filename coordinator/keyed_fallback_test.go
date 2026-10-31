package coordinator

import (
	"context"
	"fmt"
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
	return c, log
}

const keyedSumQuery = `cat /log/vimgym/api/main.log | awk '{ s[$1] += $2 } END { for (k in s) print k, s[k] }'`

func runKeyed(t *testing.T, c *Coordinator, query string) string {
	t.Helper()
	res, err := c.Execute(context.Background(), query, fullRange, "kevlar", "")
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

// One shard without the facts keeps the stage in compute.
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

// The facts are a guess: a float in a field they called integer makes the
// shard's sum unprovable, so the coordinator runs the base plan and the user
// gets awk's answer, not an error.
func TestKeyedAggregate_FallsBackWhenTheFactsWereWrong(t *testing.T) {
	both := []string{"$2", "$2#int"}
	c, log := keyedCoordinator(t, []string{"1", "2", "3"}, []string{"10", "2.5", "30"}, both, both)
	if got, want := runKeyed(t, c, keyedSumQuery), "a 44\nb 4.5\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
	if len(log.reqs) != 2 {
		t.Fatalf("%d requests, want the exact one and then the base plan", len(log.reqs))
	}
	if log.reqs[0].Group == nil || log.reqs[1].Group != nil || len(log.reqs[1].Stages) != 1 {
		t.Errorf("requests: first group %v, second group %v stages %v", log.reqs[0].Group, log.reqs[1].Group, log.reqs[1].Stages)
	}
}

// The same for a max, whose fact is only that the field is a number.
func TestKeyedAggregate_MaxFallsBackOnText(t *testing.T) {
	c, log := keyedCoordinator(t, []string{"1", "2", "3"}, []string{"10", "oops", "30"}, []string{"$2"}, []string{"$2"})
	query := `cat /log/vimgym/api/main.log | awk '{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }'`
	// "oops" compares as text against numbers in awk, which is why the plan
	// needed the fact; the base plan's answer is whatever awk says.
	got := runKeyed(t, c, query)
	if len(log.reqs) != 2 {
		t.Fatalf("%d requests, want the exact one and then the base plan (output %q)", len(log.reqs), got)
	}
}
