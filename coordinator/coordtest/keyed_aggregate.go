//go:build test

package coordtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// requestLog records every request the coordinator sends to compute.
type requestLog struct {
	inner compute.Executor
	mu    sync.Mutex
	reqs  []*pb.ExecuteQueryRequest
	ctx   context.Context // of the latest request
}

func (r *requestLog) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.ctx = ctx
	r.mu.Unlock()
	return r.inner.ExecuteQuery(ctx, req)
}

// KeyedAggregates runs a keyed awk sum over two shards: it runs on the shards
// when every one has told the coordinator its field is typed, and in compute
// when any has not, and a failing shard fails the query either way.
func KeyedAggregates(t *testing.T, newSubject func(t *testing.T) Subject) {
	both := []string{"$2", "$2#int"}
	v0, v1 := []string{"1", "2", "3"}, []string{"10", "20", "30"}

	// With the facts on every shard the sum runs in the shards: one request, a
	// group and no stage.
	t.Run("RunsOnTheShardsWhenEveryShardHasTheFacts", func(t *testing.T) {
		f, log, _ := keyedFixture(t, newSubject(t), v0, v1, both, both)
		if got, want := f.exec(t, keyedSumQuery, f.full()).Text, "a 44\nb 22\n"; got != want {
			t.Errorf("output %q, want %q", got, want)
		}
		if len(log.reqs) != 1 {
			t.Fatalf("%d requests, want 1", len(log.reqs))
		}
		if req := log.reqs[0]; req.Group.GetRender() != pb.GroupRender_GROUP_RENDER_KEY_SUM || len(req.Stages) != 0 {
			t.Errorf("request group %v stages %v, want the shard's GROUP BY and no stage", req.Group, req.Stages)
		}
	})

	// One shard without the facts keeps the stage in compute for the whole query:
	// the plan is exact for the shards whose facts say so, and there is no other
	// kind of answer to merge it with.
	t.Run("BasePlanWhenAShardLacksTheFacts", func(t *testing.T) {
		f, log, _ := keyedFixture(t, newSubject(t), v0, v1, both, nil)
		if got, want := f.exec(t, keyedSumQuery, f.full()).Text, "a 44\nb 22\n"; got != want {
			t.Errorf("output %q, want %q", got, want)
		}
		if len(log.reqs) != 1 || log.reqs[0].Group != nil || len(log.reqs[0].Stages) != 1 {
			t.Errorf("requests %v, want one with the stage and no group", log.reqs)
		}
	})

	// One failing shard fails the whole query: no partial answer from the others,
	// for a plan that runs on the shards and for the one that runs in compute.
	for name, facts := range map[string][2][]string{"exact plan": {both, both}, "base plan": {both, nil}} {
		t.Run("AFailingShardFailsTheQuery/"+name, func(t *testing.T) {
			f, _, shards := keyedFixture(t, newSubject(t), v0, v1, facts[0], facts[1])
			if err := shards[1].Close(); err != nil {
				t.Fatal(err)
			}
			res, err := f.c.Execute(context.Background(), keyedSumQuery, f.full(), f.s.ActorOf(""))
			if err == nil {
				d, derr := res.Drain(context.Background())
				if derr == nil {
					t.Fatalf("a query over a failing shard returned %q", d.Text)
				}
			}
		})
	}
}

const keyedSumQuery = `cat /log/vimgym/api/main.log | awk '{ s[$1] += $2 } END { for (k in s) print k, s[k] }'`

// keyedFixture is two shards of `key value` lines for vimgym/api/main, the
// second's values given by values1, and the facts each has told the coordinator
// (nil for a shard that has told it nothing).
func keyedFixture(t *testing.T, s Subject, values0, values1, facts0, facts1 []string) (*queryFixture, *requestLog, []*shard.DuckDBShard) {
	t.Helper()
	f := newBareFixture(t, s)
	var customer int64
	if s.Stamp != nil {
		probe := &pb.LogRow{}
		s.Stamp(probe)
		customer = probe.CustomerId
	}
	var shards []*shard.DuckDBShard
	for i, spec := range []struct {
		values []string
		facts  []string
		start  time.Time
	}{{values0, facts0, f.t0}, {values1, facts1, f.t1}} {
		sh, err := shard.NewDuckDBShard(fmt.Sprintf("keyed-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sh.Close() })
		shards = append(shards, sh)
		var rows []*pb.LogRow
		for j, v := range spec.values {
			r := &pb.LogRow{
				Raw: fmt.Sprintf("%s %s", []string{"a", "b"}[j%2], v), TsUnixNs: spec.start.Add(time.Duration(j+1) * time.Second).UnixNano(),
				Namespace: "vimgym", Service: "api", Kind: "main",
			}
			if s.Stamp != nil {
				s.Stamp(r)
			}
			rows = append(rows, r)
		}
		if err := sh.Ingest(context.Background(), rows, nil); err != nil {
			t.Fatal(err)
		}
		f.registerServing(t, &logdb.Shard{
			ShardRow: logdb.ShardRow{ID: sh.ID(), WindowStart: spec.start, WindowEnd: spec.start.Add(time.Hour), State: logdb.StateIngesting, CreatedAt: time.Now()},
			Live:     sh,
		}, "vimgym/api")
		var deltas []*pb.FieldFactDelta
		for n, loc := range spec.facts {
			deltas = append(deltas, &pb.FieldFactDelta{
				CustomerId: customer, Namespace: "vimgym", Service: "api", Kind: "main", Locator: loc,
				Table: s.FactTable("vimgym", "api", "main"),
				State: pb.FieldFactState_FIELD_FACT_STATE_TYPED, Epoch: uint64(n + 1),
			})
		}
		f.c.Provisioner.FoldFieldFacts(sh.ID(), deltas)
	}
	log := &requestLog{inner: f.c.Compute}
	f.c.Compute = log
	return f, log, shards
}

// TableQueries checks the table the coordinator sends compute: the one the
// plan names for a single source, none for a query over several.
func TableQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	t.Run("SendsTheTableTheSourceNames", func(t *testing.T) {
		s := newSubject(t)
		f, log, _ := keyedFixture(t, s, []string{"1", "2"}, []string{"3"}, nil, nil)
		got := f.exec(t, `cat /log/vimgym/api/main.log`, f.full()).Text
		if n := strings.Count(strings.TrimSpace(got), "\n") + 1; n != 3 {
			t.Fatalf("cat returned %d lines, want 3:\n%s", n, got)
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.reqs) == 0 {
			t.Fatal("no request reached compute")
		}
		want := s.FactTable("vimgym", "api", "main")
		for _, r := range log.reqs {
			if r.Table != want {
				t.Errorf("request Table = %q, want %q", r.Table, want)
			}
		}
	})

	t.Run("NamesNoTableForMoreThanOneSource", func(t *testing.T) {
		f, log, _ := keyedFixture(t, newSubject(t), []string{"1"}, []string{"2"}, nil, nil)
		f.exec(t, `cat /log/vimgym/api/main.log /log/vimgym/api/heartbeats.log`, f.full())
		log.mu.Lock()
		defer log.mu.Unlock()
		for _, r := range log.reqs {
			if r.Table != "" {
				t.Errorf("a two-source query sent Table %q, want none", r.Table)
			}
		}
	})
}
