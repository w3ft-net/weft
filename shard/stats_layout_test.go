package shard

import (
	"context"
	"reflect"
	"sort"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

func statRow(raw string, ts int64) *pb.LogRow {
	return &pb.LogRow{Raw: raw, TsUnixNs: ts * 1e9, Namespace: "ns", Service: "svc"}
}

// Archive stats group by the layout's partition columns and carry each group's
// values positionally, so a shape needs no customer_id to be summarized.
func TestCustomerStats_GroupByTheLayoutsColumns(t *testing.T) {
	lay := shape.NewLayout(
		shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
		shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
	)
	ds, err := NewDuckDBShard("s", "", WithLayout(lay))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	ctx := context.Background()
	for _, b := range []Batch{
		{Rows: []*pb.LogRow{statRow("a", 100), statRow("b", 200)}, PartitionValues: []string{"7", "gold"}},
		{Rows: []*pb.LogRow{statRow("c", 300)}, PartitionValues: []string{"7", "free"}},
	} {
		if err := ds.IngestBatch(ctx, b, nil); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := ds.customerStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, s := range stats {
		if s.CustomerID != 0 {
			t.Errorf("CustomerID = %d for a layout with no customer_id", s.CustomerID)
		}
		got[s.PartitionValues[0]+"/"+s.PartitionValues[1]] = s.RowCount
	}
	want := map[string]int64{"7/gold": 2, "7/free": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row counts by partition = %v, want %v", got, want)
	}
}

// In the legacy layout the stats still say CustomerID, and say the same thing
// positionally.
func TestCustomerStats_LegacyKeepsCustomerID(t *testing.T) {
	ds, err := NewDuckDBShard("s", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	r := statRow("a", 100)
	r.CustomerId = 42
	if err := ds.Ingest(context.Background(), []*pb.LogRow{r}, nil); err != nil {
		t.Fatal(err)
	}
	stats, err := ds.customerStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].CustomerID != 42 || !reflect.DeepEqual(stats[0].PartitionValues, []string{"42"}) {
		t.Fatalf("stats = %+v, want customer 42 with values [42]", stats)
	}
}

// The wire form carries the values, and a stat from an older shard (no values)
// reads back with none.
func TestCustomerStats_ProtoCarriesPartitionValues(t *testing.T) {
	in := []CustomerStat{{CustomerID: 9, PartitionValues: []string{"9"}, RowCount: 3}}
	out := CustomerStatsFromProto(CustomerStatsToProto(in))
	sort.Slice(out, func(i, j int) bool { return out[i].CustomerID < out[j].CustomerID })
	if !reflect.DeepEqual(out[0].PartitionValues, []string{"9"}) || out[0].CustomerID != 9 {
		t.Fatalf("round trip = %+v", out)
	}
	old := CustomerStatsFromProto([]*pb.CustomerArchiveStat{{CustomerId: 9, RowCount: 3}})
	if old[0].CustomerID != 9 || old[0].PartitionValues != nil {
		t.Fatalf("older shard's stat = %+v", old[0])
	}
}
