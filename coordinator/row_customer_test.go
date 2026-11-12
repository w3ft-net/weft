package coordinator

import (
	"context"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// The customer of a row in the coordinator is its added column, never the
// LogRow's customer_id: a row whose tag 26 is unset still belongs to customer 7.

func TestRow_CustomerIsTheAddedColumnNotTag26(t *testing.T) {
	shapetest.NeedsTenant(t)
	row := Row{LogRow: &pb.LogRow{Namespace: "ns", Service: "svc", Kind: "main", Raw: "x"}, Added: shapetest.Added(7)}
	b := NewTailBroker()
	ctx := context.Background()
	_, ch7 := b.Subscribe(ctx, tailSet(7))
	_, ch0 := b.Subscribe(ctx, tailSet(0))
	_, ch8 := b.Subscribe(ctx, tailSet(8))

	if !b.HasSubscriber(row) {
		t.Error("HasSubscriber does not see the customer-7 subscription")
	}
	b.Broadcast([]Row{row})

	if got := received(ch7); len(got) != 1 || got[0] != "x" {
		t.Errorf("customer 7's subscription got %v, want [x]", got)
	}
	if got := received(ch0); got != nil {
		t.Errorf("a customer-0 subscription got %v: the row's unset tag 26 was read as its customer", got)
	}
	if got := received(ch8); got != nil {
		t.Errorf("customer 8's subscription got %v", got)
	}
}

func TestDrain_GroupsAndPartitionsByTheAddedColumn(t *testing.T) {
	shapetest.NeedsTenant(t)
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	sh := &recordingShard{countingShard: countingShard{dlqFailingShard: dlqFailingShard{id: "shard-rec"}}}
	seedIngesting(c, sh.id, sh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	rows := RowsOf(shapetest.Added(7), dlqTestRows("weft/coordinator", 3))
	rows = append(rows, RowsOf(shapetest.Added(8), dlqTestRows("weft/coordinator", 2))...)
	for _, r := range rows {
		if r.CustomerId != 0 {
			t.Fatal("the fixture stamps tag 26")
		}
	}
	c.Ingest.Write(rows)

	var got map[string]int
	if err := waitFor(2*time.Second, func() bool {
		sh.bmu.Lock()
		defer sh.bmu.Unlock()
		got = map[string]int{}
		for _, b := range sh.batches {
			if len(b.PartitionValues) != 1 {
				return false
			}
			got[b.PartitionValues[0]] += len(b.Rows)
		}
		return len(got) == 2 && got["7"] == 3 && got["8"] == 2
	}); err != nil {
		t.Fatalf("rows landed under partitions %v, want 3 under 7 and 2 under 8", got)
	}
	if n := len(putter.Calls()); n != 0 {
		t.Errorf("%d batches went to the DLQ", n)
	}
}

// The DLQ keeps the added columns beside the row, so a replay restores the
// customer of a row that never carried tag 26.
func TestDLQ_EnvelopeRestoresTheAddedColumnsWithoutTag26(t *testing.T) {
	d, _ := testDLQ()
	rows := dlqTestRows("weft/coordinator", 2)
	if d.Write(shapetest.Added(7), rows) == "" {
		t.Fatal("Write failed")
	}
	var got []Row
	if _, err := d.Replay(context.Background(), func(r []Row) { got = append(got, r...) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("replayed %d rows, want 2", len(got))
	}
	for i, r := range got {
		if r.Added != shapetest.Added(7) || r.CustomerId != 0 {
			t.Errorf("row %d replayed as %+v with customer_id %d, want tenant 7 and 0", i, r.Added, r.CustomerId)
		}
	}
}

// An object written before the envelope holds bare LogRows; their customer is
// the row's customer_id.
func TestDLQ_ReadsALineWrittenBeforeTheEnvelope(t *testing.T) {
	line, err := protojson.Marshal(&pb.LogRow{Namespace: "weft", Service: "coordinator", Raw: "old", CustomerId: 12})
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeDLQLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if r.Raw != "old" {
		t.Errorf("legacy line decoded as row %q, want old", r.Raw)
	}
	if want := (shapetest.Added(12)); !reflect.DeepEqual(r.Added, want) {
		t.Errorf("Added = %+v, want %+v", r.Added, want)
	}
}
