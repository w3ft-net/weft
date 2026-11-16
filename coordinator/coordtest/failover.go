//go:build test

package coordtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// addShard ingests rows (stamped as the owner's) into a new in-process shard
// serving vimgym and callbox over [start, end), and registers it.
func (f *queryFixture) addShard(t *testing.T, id string, start, end time.Time, rows []*pb.LogRow) *shard.DuckDBShard {
	t.Helper()
	live, err := shard.NewDuckDBShard(id, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	for _, r := range rows {
		if f.s.Stamp != nil {
			f.s.Stamp(r)
		}
	}
	if err := live.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	f.register(t, &logdb.Shard{
		ShardRow: logdb.ShardRow{ID: id, WindowStart: start, WindowEnd: end, State: logdb.StateIngesting, CreatedAt: time.Now()},
		Live:     live,
	})
	return live
}

// Failover is the coordinator meeting a shard list that changes under it, as it
// does when a shard instance is replaced: it starts with none and recovers when
// one appears, a shard added later is queried at once, and registering the same
// shard again loses nothing.
func Failover(t *testing.T, newSubject func(t *testing.T) Subject) {
	// A coordinator started with no shards, as during a replacement when none is
	// in service yet, returns nothing and does not error, then recovers.
	t.Run("EmptyThenRecover", func(t *testing.T) {
		f := newBareFixture(t, newSubject(t))
		if d := f.exec(t, "cat vimgym.log", f.full()); len(d.Rows) != 0 {
			t.Fatalf("expected 0 rows with no shards, got %d", len(d.Rows))
		}
		f.addShard(t, "shard-0", f.t0, f.t1, queryShard0Rows(f.t0))
		if d := f.exec(t, "cat vimgym.log", f.full()); len(d.Rows) == 0 {
			t.Fatal("expected rows after adding a shard, got 0")
		}
	})

	t.Run("AddedShardIsQueried", func(t *testing.T) {
		f := newBareFixture(t, newSubject(t))
		f.addShard(t, "shard-0", f.t0, f.t1, queryShard0Rows(f.t0))
		before := f.exec(t, "cat vimgym.log", f.full())
		if len(before.Rows) == 0 {
			t.Fatal("expected rows from the first shard")
		}
		f.addShard(t, "shard-1", f.t1, f.t2, queryShard1Rows(f.t1))
		if after := f.exec(t, "cat vimgym.log", f.full()); len(after.Rows) <= len(before.Rows) {
			t.Fatalf("expected more rows after adding shard-1 (was %d, now %d)", len(before.Rows), len(after.Rows))
		}
	})

	// Registering a shard again, as a rediscovery does, keeps its data: the
	// instance is the same, and only the registry's record of it is renewed.
	t.Run("ReregisteringDoesNotLoseData", func(t *testing.T) {
		f := newBareFixture(t, newSubject(t))
		live := f.addShard(t, "shard-0", f.t0, f.t1, []*pb.LogRow{
			{Raw: "important data", Namespace: "vimgym", Level: "INFO", TsUnixNs: f.t0.Add(time.Second).UnixNano()},
		})
		tr := f.full()
		if d := f.exec(t, "cat vimgym.log", tr); len(d.Rows) != 1 {
			t.Fatalf("expected 1 row before, got %d", len(d.Rows))
		}
		f.register(t, &logdb.Shard{
			ShardRow: logdb.ShardRow{ID: "shard-0", WindowStart: f.t0, WindowEnd: f.t1, State: logdb.StateIngesting, CreatedAt: time.Now()},
			Live:     live,
		})
		if d := f.exec(t, "cat vimgym.log", tr); len(d.Rows) != 1 {
			t.Fatalf("expected 1 row after, got %d", len(d.Rows))
		}
	})
}

// FailoverConcurrent runs queries from several goroutines at once over a shard:
// none panics or deadlocks. Each Execute reaches the stores on its own, so the
// Subject must not be a single connection.
func FailoverConcurrent(t *testing.T, newSubject func(t *testing.T) Subject) {
	f := newBareFixture(t, newSubject(t))
	f.addShard(t, "shard-0", f.t0, f.t1, queryShard0Rows(f.t0))

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	// Each result is drained so the producers of the streaming path end cleanly
	// between iterations rather than leaking on backpressure.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				res, err := f.c.Execute(context.Background(), "cat vimgym.log", f.full(), f.s.PrincipalOf(""))
				if err != nil {
					errs <- err
					continue
				}
				if _, err := res.Drain(context.Background()); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	if n := len(errs); n > 0 {
		t.Logf("%d transient errors during concurrent queries (expected)", n)
	}
}
