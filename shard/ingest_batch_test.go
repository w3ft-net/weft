package shard

import (
	"context"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

func batchRow(raw string) *pb.LogRow {
	return &pb.LogRow{Raw: raw, TsUnixNs: 1_700_000_000_000_000_000, Namespace: "ns", Service: "svc"}
}

// A batch's partition values are typed once per batch and stored in the
// layout's columns, whatever the rows' own customer_id says.
func TestIngestBatch_PartitionValuesFillLayoutColumns(t *testing.T) {
	lay := shape.NewLayout(
		shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
		shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
	)
	ds, err := NewDuckDBShard("b", "", WithLayout(lay))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	b := Batch{Rows: []*pb.LogRow{batchRow("a"), batchRow("b")}, PartitionValues: []string{"7", "gold"}}
	if err := ds.IngestBatch(context.Background(), b, nil); err != nil {
		t.Fatalf("IngestBatch: %v", err)
	}
	var n int
	if err := ds.db.QueryRow(`SELECT count(*) FROM logs WHERE account_id = 7 AND tier = 'gold'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows with account_id=7 tier=gold = %d, want 2", n)
	}
}

// In the legacy layout the batch value wins over a row's tag-26 customer_id,
// and a batch with no values still reads it from the rows.
func TestIngestBatch_LegacyLayout(t *testing.T) {
	ds, err := NewDuckDBShard("l", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	ctx := context.Background()

	stale := batchRow("from-batch")
	stale.CustomerId = 1
	if err := ds.IngestBatch(ctx, Batch{Rows: []*pb.LogRow{stale}, PartitionValues: []string{"42"}}, nil); err != nil {
		t.Fatal(err)
	}
	tagged := batchRow("from-row")
	tagged.CustomerId = 9
	if err := ds.IngestBatch(ctx, Batch{Rows: []*pb.LogRow{tagged}}, nil); err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]int64{"from-batch": 42, "from-row": 9} {
		var got int64
		if err := ds.db.QueryRow(`SELECT customer_id FROM logs WHERE raw = ?`, raw).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: customer_id = %d, want %d", raw, got, want)
		}
	}
}

// A batch that cannot be stored under the layout is refused whole, never
// written with NULL partition columns.
func TestIngestBatch_RefusesBadPartitionValues(t *testing.T) {
	lay := shape.NewLayout(shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition})
	ds, err := NewDuckDBShard("r", "", WithLayout(lay))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	for name, vals := range map[string][]string{
		"missing":   nil,
		"wrong len": {"1", "2"},
		"not int":   {"abc"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ds.IngestBatch(context.Background(), Batch{Rows: []*pb.LogRow{batchRow("x")}, PartitionValues: vals}, nil)
			if err == nil {
				t.Fatal("IngestBatch succeeded, want an error")
			}
		})
	}
	var n int
	if err := ds.db.QueryRow(`SELECT count(*) FROM logs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows stored = %d (err %v), want 0", n, err)
	}
}
