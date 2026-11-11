package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

var customLayout = shape.NewLayout(
	shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
	shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
)

type batch struct {
	partition []string
	rows      []*pb.LogRow
}

func logRow(raw string, sec int64) *pb.LogRow {
	return &pb.LogRow{Raw: raw, TsUnixNs: sec * 1e9, Namespace: "ns", Service: "svc", Kind: "main"}
}

// sizedArchive stores the batches in a shard of layout, archives it, and
// converts the archive to the sized layout, the one retention rewrites. It
// returns the sized archive's glob.
func sizedArchive(t *testing.T, layout shape.Layout, batches ...batch) string {
	t.Helper()
	ds, err := shard.NewDuckDBShard("x", "", shard.WithLayout(layout))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	for _, b := range batches {
		if err := ds.IngestBatch(context.Background(), shard.Batch{Rows: b.rows, PartitionValues: b.partition}, nil); err != nil {
			t.Fatal(err)
		}
	}
	path, _, err := ds.Archive(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	glob := filepath.Join(t.TempDir(), "sized", "**", "*.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := shard.WriteArchive(db, "SELECT * FROM "+shard.ParquetReadExpr(path, layout), glob, layout, nil); err != nil {
		t.Fatal(err)
	}
	return glob
}

func goldArchive(t *testing.T) string {
	return sizedArchive(t, customLayout, batch{[]string{"7", "gold"}, []*pb.LogRow{logRow("one", 1_700_000_000), logRow("two", 1_700_000_100)}})
}

func twoPartitionArchive(t *testing.T) string {
	return sizedArchive(t, customLayout,
		batch{[]string{"7", "gold"}, []*pb.LogRow{logRow("g1", 1_700_000_000), logRow("g2", 1_700_000_100)}},
		batch{[]string{"7", "free"}, []*pb.LogRow{logRow("f1", 1_700_000_900)}})
}

// The compactor reads and stats an archive in the layout it is told, and
// rewrites it in that layout.
func TestCompactor_UsesTheArchivesLayout(t *testing.T) {
	glob := goldArchive(t)
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
	glob := twoPartitionArchive(t)
	c := LocalCompactor{Layout: customLayout}
	res, err := c.Rewrite(glob, t.TempDir()+"/o/sized/**/*.parquet", []CustomerCutoff{
		{PartitionValues: []string{"7", "gold"}, Cutoff: time.Unix(1_700_000_500, 0)},
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
	glob := twoPartitionArchive(t)
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
	glob := sizedArchive(t, none, batch{nil, []*pb.LogRow{logRow("a", 1_700_000_000)}})
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

// The request body carries the layout, and leaves it out when none was stated
// so an older handler sees the body it always did.
func TestCompactRequest_CarriesTheLayoutOnlyWhenStated(t *testing.T) {
	b, _ := json.Marshal(CompactRequest{Op: CompactOpCapture, Src: "p", Layout: customLayout})
	if !strings.Contains(string(b), `"layout"`) {
		t.Errorf("a request omits a stated layout: %s", b)
	}
	b, _ = json.Marshal(CompactRequest{Op: CompactOpCapture, Src: "p"})
	if strings.Contains(string(b), `"layout"`) {
		t.Errorf("a request carries an unstated layout: %s", b)
	}
}

// A retention rewrite of a sized archive writes a sized archive. Written as one
// file it would put every service back in every file and drop the bucket width.
func TestRewriteRetention_KeepsASizedLayout(t *testing.T) {
	// Customer 1 has rows 7 hours apart from 2026-10-14 00:00 in each of five
	// services; customer 2 has the same in one.
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC).Unix()
	rowsFor := func(svc string) []*pb.LogRow {
		var rows []*pb.LogRow
		for i := int64(0); i < 6; i++ {
			r := logRow(svc, start+i*7*3600)
			r.Service = svc
			rows = append(rows, r)
		}
		return rows
	}
	var batches []batch
	for _, svc := range []string{"a", "b", "c", "d", "e"} {
		batches = append(batches, batch{[]string{"1"}, rowsFor(svc)})
	}
	batches = append(batches, batch{[]string{"2"}, rowsFor("a")})
	src := sizedArchive(t, shape.Legacy(), batches...)

	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	dst := dstDir + "/sized/**/*.parquet"
	// The two rows before 14:00 go, in each of customer 1's 5 services.
	cutoff := time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)
	res, err := RewriteRetention(src, dst, []CustomerCutoff{{CustomerID: 1, Cutoff: cutoff}}, shape.Legacy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsKept != 5*4+6 || res.RowsDeleted != 5*2 {
		t.Fatalf("kept %d, deleted %d; want 26 and 10", res.RowsKept, res.RowsDeleted)
	}
	if res.OutputPath != dst || !shard.IsKeyed(res.OutputPath) || !shard.IsSized(res.OutputPath) {
		t.Fatalf("output path %q is not the sized destination %q", res.OutputPath, dst)
	}
	files, _ := filepath.Glob(filepath.Join(dstDir, "sized", "customer_id=1/namespace=ns/service=a/kind=main", "_w=*", "*", "data_0.parquet"))
	if len(files) == 0 {
		t.Error("the rewrite wrote no directory for ns/a/main")
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n, early int
	q := "SELECT count(*), count(*) FILTER (WHERE customer_id = 1 AND ts < TIMESTAMP '2026-10-14 14:00:00') FROM " + shard.ParquetReadExpr(dst, shape.Legacy())
	if err := db.QueryRow(q).Scan(&n, &early); err != nil {
		t.Fatal(err)
	}
	if n != 26 || early != 0 {
		t.Errorf("rewritten archive holds %d rows, %d of them expired; want 26 and 0", n, early)
	}
}
