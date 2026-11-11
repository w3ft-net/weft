package shard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// The sized archive's directories are the layout's partition columns, in
// order, then the key and the time bucket, whatever the shape.
func TestExportKeyedParquet_DirectoriesFollowTheLayout(t *testing.T) {
	lay := shape.NewLayout(
		shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
		shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
	)
	ds, err := NewDuckDBShard("x", "", WithLayout(lay))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	row := &pb.LogRow{Raw: "hello", TsUnixNs: 1_700_000_000 * 1e9, Namespace: "ns", Service: "svc", Kind: "main"}
	if err := ds.IngestBatch(context.Background(), Batch{Rows: []*pb.LogRow{row}, PartitionValues: []string{"7", "gold"}}, nil); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir() + "/shard.attempt-1"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, n, err := ds.exportKeyedParquet(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 1 {
		t.Fatalf("exported %d rows, want 1", n)
	}
	var found string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".parquet") {
			found = strings.TrimPrefix(p, dir+"/sized/")
		}
		return nil
	})
	if !strings.HasPrefix(found, "account_id=7/tier=gold/namespace=ns/service=svc/kind=main/_w=") {
		t.Fatalf("archive path = %q, want account_id=7/tier=gold/namespace=ns/service=svc/kind=main/_w=…", found)
	}
}

// The export host reads the layout from the volume's shard.json, so an archive
// is written in the layout its data was ingested with.
func TestOpenForExport_ReadsTheVolumesLayout(t *testing.T) {
	lay := shape.NewLayout(shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition})
	dir := t.TempDir()
	if err := SaveIdentity(dir, &Identity{ShardID: "x", Layout: lay}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "shard.duckdb")
	ds, err := NewDuckDBShard("x", path, WithLayout(lay))
	if err != nil {
		t.Fatal(err)
	}
	ds.Close()

	e, err := OpenForExport("x", path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.layout.Fingerprint != lay.Fingerprint {
		t.Fatalf("export layout = %+v, want %+v", e.layout, lay)
	}
}

// The legacy layout writes exactly the directories it always has.
func TestPartitionBy_LegacyIsUnchanged(t *testing.T) {
	if got, want := partitionBy(shape.Legacy()), "customer_id, namespace, service, kind, _w, _part"; got != want {
		t.Fatalf("partitionBy(legacy) = %q, want %q", got, want)
	}
}
