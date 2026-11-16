//go:build test

package coordtest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// RangeQueries runs a query over a window spanning a live shard and an archived
// one read from Parquet: both contribute rows.
func RangeQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	t.Run("SpansLiveAndColdShards", func(t *testing.T) { rangeSpansLiveAndCold(t, newBareFixture(t, newSubject(t))) })
}

func rangeSpansLiveAndCold(t *testing.T, f *queryFixture) {
	row := func(raw, level string, at time.Time) *pb.LogRow {
		r := &pb.LogRow{Raw: raw, Namespace: "vimgym", Level: level, TsUnixNs: at.UnixNano()}
		if f.s.Stamp != nil {
			f.s.Stamp(r)
		}
		return r
	}

	live, err := shard.NewDuckDBShard("shard-live", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	if err := live.Ingest(context.Background(), []*pb.LogRow{
		row("live row 1", "INFO", f.t1.Add(1*time.Second)),
		row("live row 2", "ERROR", f.t1.Add(2*time.Second)),
	}, nil); err != nil {
		t.Fatal(err)
	}

	src, err := shard.NewDuckDBShard("shard-cold-src", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Ingest(context.Background(), []*pb.LogRow{
		row("cold row 1", "INFO", f.t0.Add(1*time.Second)),
		row("cold row 2", "ERROR", f.t0.Add(2*time.Second)),
		row("cold row 3", "WARN", f.t0.Add(3*time.Second)),
	}, nil); err != nil {
		t.Fatal(err)
	}
	parquet := filepath.Join(t.TempDir(), "cold.parquet")
	if err := src.ExportToParquet(parquet); err != nil {
		t.Fatal(err)
	}
	src.Close()
	cold, err := shard.NewColdShard("shard-cold", []string{"vimgym"}, parquet, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cold.Close() })

	f.register(t, &logdb.Shard{
		ShardRow: logdb.ShardRow{ID: "shard-live", WindowStart: f.t1, WindowEnd: f.t2, State: logdb.StateIngesting, CreatedAt: time.Now()},
		Live:     live,
	})
	f.register(t, &logdb.Shard{
		ShardRow: logdb.ShardRow{ID: "shard-cold", WindowStart: f.t0, WindowEnd: f.t1, State: logdb.StateArchived, RowCount: 3, CreatedAt: time.Now()},
		Live:     cold,
	})

	d := f.exec(t, "cat vimgym.log", f.full())
	// Ordering across shards is by line_num, which is per shard, so compare as a set.
	got := map[string]bool{}
	for _, r := range d.Rows {
		got[r.Raw] = true
	}
	if len(d.Rows) != 5 {
		t.Fatalf("want 5 rows (3 cold + 2 live), got %d", len(d.Rows))
	}
	for _, want := range []string{"cold row 1", "cold row 2", "cold row 3", "live row 1", "live row 2"} {
		if !got[want] {
			t.Errorf("missing row %q", want)
		}
	}
}
