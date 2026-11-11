package shard

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

func statRow(raw string, ts int64) *pb.LogRow {
	return &pb.LogRow{Raw: raw, TsUnixNs: ts * 1e9, Namespace: "ns", Service: "svc"}
}

var accountTier = shape.NewLayout(
	shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
	shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
)

// shardOfAccountTier is a shard of a layout with no customer_id, holding three
// groups: 7/gold twice, 7/free once and 8/gold once.
func shardOfAccountTier(t *testing.T) *DuckDBShard {
	t.Helper()
	ds, err := NewDuckDBShard("s", "", WithLayout(accountTier))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	for _, b := range []Batch{
		{Rows: []*pb.LogRow{statRow("a", 100), statRow("b", 200)}, PartitionValues: []string{"7", "gold"}},
		{Rows: []*pb.LogRow{statRow("c", 300)}, PartitionValues: []string{"7", "free"}},
		{Rows: []*pb.LogRow{statRow("d", 400)}, PartitionValues: []string{"8", "gold"}},
	} {
		if err := ds.IngestBatch(context.Background(), b, nil); err != nil {
			t.Fatal(err)
		}
	}
	return ds
}

// Archive stats group by the layout's partition columns and carry each group's
// values positionally, so a shape needs no customer_id to be summarized.
func TestPartitionStats_GroupByTheLayoutsColumns(t *testing.T) {
	stats, err := shardOfAccountTier(t).archiveStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, s := range stats {
		if s.LegacyCustomerID != 0 {
			t.Errorf("LegacyCustomerID = %d for a layout with no customer_id", s.LegacyCustomerID)
		}
		got[s.PartitionValues[0]+"/"+s.PartitionValues[1]] = s.RowCount
	}
	want := map[string]int64{"7/gold": 2, "7/free": 1, "8/gold": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row counts by partition = %v, want %v", got, want)
	}
}

// In the legacy layout the stats still say the customer as a number, for a
// reader of the previous build, and say the same thing positionally.
func TestPartitionStats_LegacyKeepsTheCustomerNumber(t *testing.T) {
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
	stats, err := ds.archiveStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].LegacyCustomerID != 42 || !reflect.DeepEqual(stats[0].PartitionValues, []string{"42"}) {
		t.Fatalf("stats = %+v, want customer 42 with values [42]", stats)
	}
}

// The wire form carries the values and the customer number, and a stat from an
// older shard, which sent the customer number alone, reads back with the values
// a current one sends.
func TestPartitionStats_ProtoCarriesPartitionValues(t *testing.T) {
	in := []PartitionStat{{PartitionValues: []string{"9"}, LegacyCustomerID: 9, RowCount: 3}}
	wire := PartitionStatsToProto(in)
	if wire[0].CustomerId != 9 || !reflect.DeepEqual(wire[0].PartitionValues, []string{"9"}) {
		t.Fatalf("wire = %+v, want customer_id 9 and values [9]", wire[0])
	}
	if out := PartitionStatsFromProto(wire); !reflect.DeepEqual(out, []PartitionStat{{PartitionValues: []string{"9"}, LegacyCustomerID: 9, MinTs: out[0].MinTs, MaxTs: out[0].MaxTs, RowCount: 3}}) {
		t.Fatalf("round trip = %+v", out)
	}
	old := PartitionStatsFromProto([]*pb.CustomerArchiveStat{{CustomerId: 9, RowCount: 3}})
	if old[0].LegacyCustomerID != 9 || !reflect.DeepEqual(old[0].PartitionValues, []string{"9"}) {
		t.Fatalf("older shard's stat = %+v, want values [9]", old[0])
	}
	// Neither a customer nor values is a layout with no partition columns; it is
	// not guessed to be customer 0.
	none := PartitionStatsFromProto([]*pb.CustomerArchiveStat{{RowCount: 3}})
	if len(none[0].PartitionValues) != 0 {
		t.Fatalf("a stat with no customer and no values got values %v", none[0].PartitionValues)
	}
}

func TestPartitionStat_JSONRoundTripsAndReadsTheOldForm(t *testing.T) {
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	b, err := json.Marshal(PartitionStat{PartitionValues: []string{"7"}, LegacyCustomerID: 7, MinTs: at, MaxTs: at, RowCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	// the previous build reads customer_id, and ignores the rest
	if !strings.Contains(string(b), `"customer_id":7`) || !strings.Contains(string(b), `"partition_values":["7"]`) {
		t.Errorf("stat json = %s", b)
	}
	var back PartitionStat
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back.PartitionValues, []string{"7"}) || back.LegacyCustomerID != 7 || back.RowCount != 3 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}

	old := `{"customer_id":7,"min_ts":"2026-05-01T12:00:00Z","max_ts":"2026-05-01T12:00:00Z","row_count":3}`
	var fromOld PartitionStat
	if err := json.Unmarshal([]byte(old), &fromOld); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromOld.PartitionValues, []string{"7"}) || fromOld.LegacyCustomerID != 7 || !fromOld.MinTs.Equal(at) || fromOld.RowCount != 3 {
		t.Errorf("old-form stat = %+v, want values [7]", fromOld)
	}
}

// A manifest keeps the "customers" key the previous build reads and writes, in
// both directions.
func TestExportManifest_KeepsTheKeyAndTheCustomerNumberForTheOldBuild(t *testing.T) {
	b, err := json.Marshal(ExportManifest{ShardID: "s", Partitions: []PartitionStat{{PartitionValues: []string{"5"}, LegacyCustomerID: 5, RowCount: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Customers []struct {
			CustomerID int64 `json:"customer_id"`
			RowCount   int64 `json:"row_count"`
		} `json:"customers"`
	}
	if err := json.Unmarshal(b, &generic); err != nil || len(generic.Customers) != 1 || generic.Customers[0].CustomerID != 5 || generic.Customers[0].RowCount != 7 {
		t.Fatalf("the previous build reads %s as %+v, %v", b, generic, err)
	}

	var m ExportManifest
	old := `{"shard_id":"s","rows":7,"customers":[{"customer_id":5,"row_count":7,"min_ts":"2026-05-01T00:00:00Z","max_ts":"2026-05-01T00:00:00Z"}]}`
	if err := json.Unmarshal([]byte(old), &m); err != nil || len(m.Partitions) != 1 || !reflect.DeepEqual(m.Partitions[0].PartitionValues, []string{"5"}) {
		t.Fatalf("an old manifest decodes to %+v, %v", m, err)
	}
}
