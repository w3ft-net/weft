package shard

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

var customLayout = shape.NewLayout(
	shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
	shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
)

// exportCustomArchive writes a sized archive of two rows in customLayout and
// returns its glob.
func exportCustomArchive(t *testing.T) string {
	t.Helper()
	ds, err := NewDuckDBShard("x", "", WithLayout(customLayout))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	rows := []*pb.LogRow{
		{Raw: "one", TsUnixNs: 1_700_000_000 * 1e9, Namespace: "ns", Service: "svc", Kind: "main"},
		{Raw: "two", TsUnixNs: 1_700_000_100 * 1e9, Namespace: "ns", Service: "svc", Kind: "main"},
	}
	if err := ds.IngestBatch(context.Background(), Batch{Rows: rows, PartitionValues: []string{"7", "gold"}}, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() + "/shard.attempt-1"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	glob, _, err := ds.exportKeyedParquet(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return glob
}

// exportTwoPartitionArchive writes a sized archive in customLayout with two
// partitions: [7 gold] with two rows at 1_700_000_000s and 100s on, and
// [7 free] with one at 1_700_000_900s.
func exportTwoPartitionArchive(t *testing.T) string {
	t.Helper()
	ds, err := NewDuckDBShard("x", "", WithLayout(customLayout))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	row := func(raw string, sec int64) *pb.LogRow {
		return &pb.LogRow{Raw: raw, TsUnixNs: sec * 1e9, Namespace: "ns", Service: "svc", Kind: "main"}
	}
	ctx := context.Background()
	for _, b := range []Batch{
		{Rows: []*pb.LogRow{row("g1", 1_700_000_000), row("g2", 1_700_000_100)}, PartitionValues: []string{"7", "gold"}},
		{Rows: []*pb.LogRow{row("f1", 1_700_000_900)}, PartitionValues: []string{"7", "free"}},
	} {
		if err := ds.IngestBatch(ctx, b, nil); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir() + "/shard.attempt-1"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	glob, _, err := ds.exportKeyedParquet(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return glob
}

// A cold shard told the archive's layout reads its partition columns, typed.
func TestColdShard_ReadsAnArchiveInItsLayout(t *testing.T) {
	glob := exportCustomArchive(t)
	cs, err := NewColdShard("c", nil, glob, "", nil, ColdLayout(customLayout))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE account_id = 7 AND tier = 'gold'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

// The compactor reads and stats an archive in the layout it is told, and
// rewrites it in that layout.
func TestCompactor_UsesTheArchivesLayout(t *testing.T) {
	glob := exportCustomArchive(t)
	c := LocalCompactor{Layout: customLayout}

	stats, err := c.CaptureStats(glob)
	if err != nil {
		t.Fatalf("CaptureStats: %v", err)
	}
	if len(stats) != 1 || !reflect.DeepEqual(stats[0].PartitionValues, []string{"7", "gold"}) || stats[0].RowCount != 2 {
		t.Fatalf("stats = %+v, want one group [7 gold] of 2 rows", stats)
	}

	dst := t.TempDir() + "/out/sized/**/*.parquet"
	res, err := c.Rewrite(glob, dst, nil)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if res.RowsKept != 2 || res.RowsDeleted != 0 {
		t.Fatalf("rewrite kept %d deleted %d, want 2 and 0", res.RowsKept, res.RowsDeleted)
	}
	if got, err := c.CaptureStats(dst); err != nil || len(got) != 1 || !reflect.DeepEqual(got[0].PartitionValues, []string{"7", "gold"}) {
		t.Fatalf("rewritten archive stats = %+v (err %v), want the same group", got, err)
	}
}

// A cutoff names the partition it applies to by values, in layout order, so
// retention works in any layout; a partition with no cutoff is kept whole.
func TestCompactor_CutoffsByPartitionValues(t *testing.T) {
	glob := exportTwoPartitionArchive(t)
	c := LocalCompactor{Layout: customLayout}
	after := time.Unix(1_700_000_500, 0)
	res, err := c.Rewrite(glob, t.TempDir()+"/o/sized/**/*.parquet", []CustomerCutoff{
		{PartitionValues: []string{"7", "gold"}, Cutoff: after},
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if res.RowsDeleted != 2 || res.RowsKept != 1 {
		t.Fatalf("deleted %d kept %d, want the gold partition's 2 rows deleted and free's 1 kept", res.RowsDeleted, res.RowsKept)
	}
	if len(res.Stats) != 1 || !reflect.DeepEqual(res.Stats[0].PartitionValues, []string{"7", "free"}) {
		t.Fatalf("survivor stats = %+v, want only [7 free]", res.Stats)
	}
}

// A cutoff that names no partition cannot be applied to a layout that has no
// customer_id, and says so.
func TestCompactor_RefusesACutoffItCannotPlace(t *testing.T) {
	glob := exportTwoPartitionArchive(t)
	c := LocalCompactor{Layout: customLayout}
	_, err := c.Rewrite(glob, t.TempDir()+"/o/sized/**/*.parquet", []CustomerCutoff{{CustomerID: 7}})
	if err == nil || !strings.Contains(err.Error(), "partition values") {
		t.Fatalf("err = %v, want a refusal naming partition values", err)
	}
}

// A layout with no partition columns has one partition, and a cutoff with no
// values applies to all of it.
func TestCompactor_CutoffInALayoutWithNoColumns(t *testing.T) {
	none := shape.NewLayout()
	ds, err := NewDuckDBShard("n", "", WithLayout(none))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	rows := []*pb.LogRow{{Raw: "a", TsUnixNs: 1_700_000_000 * 1e9, Namespace: "ns", Service: "svc", Kind: "main"}}
	// A layout with no columns has no values to send: nil is complete.
	if err := ds.IngestBatch(context.Background(), Batch{Rows: rows}, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() + "/shard.attempt-1"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	glob, _, err := ds.exportKeyedParquet(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := LocalCompactor{Layout: none}
	res, err := c.Rewrite(glob, t.TempDir()+"/o/sized/**/*.parquet", []CustomerCutoff{
		{PartitionValues: []string{}, Cutoff: time.Unix(1_700_000_500, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsDeleted != 1 || res.RowsKept != 0 {
		t.Fatalf("deleted %d kept %d, want the one row deleted", res.RowsDeleted, res.RowsKept)
	}
}

// The request bodies carry the layout, and leave it out when none was stated
// so an older handler sees the body it always did.
func TestRequests_CarryTheLayoutOnlyWhenStated(t *testing.T) {
	for name, req := range map[string]any{
		"query":   LambdaQueryRequest{SQL: "x", ParquetPath: "p", Layout: customLayout},
		"compact": CompactRequest{Op: CompactOpCapture, Src: "p", Layout: customLayout},
	} {
		b, _ := json.Marshal(req)
		if !strings.Contains(string(b), `"layout"`) {
			t.Errorf("%s request omits a stated layout: %s", name, b)
		}
	}
	for name, req := range map[string]any{
		"query":   LambdaQueryRequest{SQL: "x", ParquetPath: "p"},
		"compact": CompactRequest{Op: CompactOpCapture, Src: "p"},
	} {
		b, _ := json.Marshal(req)
		if strings.Contains(string(b), `"layout"`) {
			t.Errorf("%s request carries an unstated layout: %s", name, b)
		}
	}
}
