package shard

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
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

// The query request carries the layout, and leaves it out when none was stated
// so an older handler sees the body it always did.
func TestQueryRequest_CarriesTheLayoutOnlyWhenStated(t *testing.T) {
	for name, req := range map[string]any{
		"query": LambdaQueryRequest{SQL: "x", ParquetPath: "p", Layout: customLayout},
	} {
		b, _ := json.Marshal(req)
		if !strings.Contains(string(b), `"layout"`) {
			t.Errorf("%s request omits a stated layout: %s", name, b)
		}
	}
	for name, req := range map[string]any{
		"query": LambdaQueryRequest{SQL: "x", ParquetPath: "p"},
	} {
		b, _ := json.Marshal(req)
		if strings.Contains(string(b), `"layout"`) {
			t.Errorf("%s request carries an unstated layout: %s", name, b)
		}
	}
}
