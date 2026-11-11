package service

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// Over the wire: Configure sets the layout, and an Ingest carrying
// partition_values stores them on every row, typed per the layout, with no
// customer_id on the rows at all.
func TestIngest_PartitionValuesOverGRPC(t *testing.T) {
	lay := shape.NewLayout(
		shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
		shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
	)
	dir := t.TempDir()
	srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
	c := serve(t, srv)
	ctx := context.Background()

	if _, err := c.Configure(ctx, &pb.ConfigureRequest{ShardId: "s1", Layout: shard.LayoutToProto(lay)}); err != nil {
		t.Fatal(err)
	}
	rows := []*pb.LogRow{{Raw: "hello world", Namespace: "acme", Service: "api", TsUnixNs: 1_700_000_000 * 1e9}}
	if _, err := c.Ingest(ctx, &pb.IngestRequest{Rows: rows, PartitionValues: []string{"7", "gold"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// A batch with none is refused: this layout has no customer_id to fall back on.
	if _, err := c.Ingest(ctx, &pb.IngestRequest{Rows: rows}); err == nil {
		t.Fatal("Ingest without partition values succeeded under a non-legacy layout")
	}

	stream, err := c.Query(ctx, &pb.QueryRequest{Sql: `SELECT raw, ts FROM logs WHERE account_id = 7 AND tier = 'gold'`})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("rows with account_id=7 tier=gold = %d, want 1", n)
	}
}
