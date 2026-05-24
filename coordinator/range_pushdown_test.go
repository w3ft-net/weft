package coordinator

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func TestCoordinator_PlannerInjectsTimeRange(t *testing.T) {
	// Live shard with recent data.
	live, err := shard.NewDuckDBShard("shard-live", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })

	liveRows := []*pb.LogRow{
		{Raw: "live row 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t1.Add(1 * time.Second).UnixNano()},
		{Raw: "live row 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t1.Add(2 * time.Second).UnixNano()},
	}
	if err := live.Ingest(liveRows, nil); err != nil {
		t.Fatal(err)
	}

	// Cold shard with older data (exported to Parquet).
	coldDS, err := shard.NewDuckDBShard("shard-cold-src", "")
	if err != nil {
		t.Fatal(err)
	}
	coldRows := []*pb.LogRow{
		{Raw: "cold row 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{Raw: "cold row 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{Raw: "cold row 3", App: "vimgym", Level: "WARN", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
	}
	if err := coldDS.Ingest(coldRows, nil); err != nil {
		t.Fatal(err)
	}

	parquetPath := filepath.Join(t.TempDir(), "cold.parquet")
	if err := coldDS.ExportToParquet(parquetPath); err != nil {
		t.Fatal(err)
	}
	coldDS.Close()

	cold, err := shard.NewColdShard("shard-cold", []string{"vimgym"}, parquetPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cold.Close() })

	// Build coordinator with both shards.
	c := NewCoordinator(CoordinatorConfig{Store: testQueryStore(t, "kevlar", "vimgym")})
	addInProcessShard(t, c, &logdb.ShardEntry{
		ID:          "shard-live",
		WindowStart: t1, WindowEnd: t2, State: logdb.StateIngesting, Shard: live,
	}, "vimgym")
	addInProcessShard(t, c, &logdb.ShardEntry{
		ID:          "shard-cold",
		WindowStart: t0, WindowEnd: t1, State: logdb.StateArchived, Shard: cold,
		RowCount: 3,
	}, "vimgym")

	// Query spanning both windows.
	result, err := c.Execute("cat vimgym.log", TimeRange{Start: t0, End: t2}, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resultD := drainExec(t, result)

	// Should get all 5 rows (3 cold + 2 live), sorted by timestamp.
	if len(resultD.Rows) != 5 {
		t.Fatalf("expected 5 rows (3 cold + 2 live), got %d", len(resultD.Rows))
	}

	// Verify all rows are present (ordering across shards is by line_num,
	// which is per-shard — not globally chronological).
	rawSet := map[string]bool{}
	for _, r := range resultD.Rows {
		rawSet[r.Raw] = true
	}
	for _, want := range []string{"cold row 1", "cold row 2", "cold row 3", "live row 1", "live row 2"} {
		if !rawSet[want] {
			t.Errorf("missing expected row %q", want)
		}
	}
}
