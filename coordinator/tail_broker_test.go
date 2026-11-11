package coordinator

import (
	"context"
	"slices"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

func tailSet(customers ...int64) *RowMatcher {
	var s shape.Set
	for _, c := range customers {
		s = append(s, shape.Coord{Customer: c})
	}
	return &RowMatcher{Coords: s}
}

func tailRow(customer int64, raw string) *pb.LogRow {
	return &pb.LogRow{CustomerId: customer, Namespace: "ns", Service: "svc", Kind: "main", Raw: raw}
}

// received is what a subscription's channel holds now.
func received(ch <-chan *pb.LogRow) []string {
	var out []string
	for {
		select {
		case r := <-ch:
			if r == nil {
				return out
			}
			out = append(out, r.Raw)
		default:
			return out
		}
	}
}

// Each subscription gets exactly the rows its coordinates admit, in order, no
// matter how many other customers' subscriptions and rows are in the stream.
func TestTailBroker_DeliversOnlyWhatEachSubscriptionAdmits(t *testing.T) {
	b := NewTailBroker()
	ctx := context.Background()
	_, ch1 := b.Subscribe(ctx, tailSet(1))
	_, ch2 := b.Subscribe(ctx, tailSet(2))
	_, ch12 := b.Subscribe(ctx, tailSet(1, 2))   // spans two customers
	_, chNone := b.Subscribe(ctx, &RowMatcher{}) // an empty set admits nothing

	b.Broadcast([]*pb.LogRow{
		tailRow(1, "a1"), tailRow(2, "b1"), tailRow(3, "c1"), tailRow(1, "a2"), tailRow(9, "z"),
	})

	for name, tc := range map[string]struct {
		ch   <-chan *pb.LogRow
		want []string
	}{
		"customer 1":        {ch1, []string{"a1", "a2"}},
		"customer 2":        {ch2, []string{"b1"}},
		"customers 1 and 2": {ch12, []string{"a1", "b1", "a2"}},
		"empty set":         {chNone, nil},
	} {
		if got := received(tc.ch); !slices.Equal(got, tc.want) {
			t.Errorf("%s got %v, want %v", name, got, tc.want)
		}
	}
}

// A subscription whose tuples name one customer twice still gets each row once,
// and only the rows some tuple admits.
func TestTailBroker_AdmitsByTheWholeTupleOnceEach(t *testing.T) {
	b := NewTailBroker()
	_, ch := b.Subscribe(context.Background(), &RowMatcher{Coords: shape.Set{
		{Customer: 3, Service: shape.Value("a")},
		{Customer: 3, Service: shape.Value("b")},
	}})
	row := func(svc, raw string) *pb.LogRow { return &pb.LogRow{CustomerId: 3, Service: svc, Raw: raw} }
	b.Broadcast([]*pb.LogRow{row("a", "1"), row("c", "2"), row("b", "3")})
	if got := received(ch); !slices.Equal(got, []string{"1", "3"}) {
		t.Fatalf("got %v, want [1 3]", got)
	}
}

func TestTailBroker_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewTailBroker()
	id, ch := b.Subscribe(context.Background(), tailSet(1))
	_, other := b.Subscribe(context.Background(), tailSet(1))
	b.Unsubscribe(id)
	b.Broadcast([]*pb.LogRow{tailRow(1, "x")})
	if _, open := <-ch; open {
		t.Fatal("an unsubscribed channel is still open")
	}
	if got := received(other); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("the other subscription got %v, want [x]", got)
	}
	b.Unsubscribe(id) // twice is harmless
}

func TestTailBroker_HasSubscriber(t *testing.T) {
	b := NewTailBroker()
	ctx, cancel := context.WithCancel(context.Background())
	b.Subscribe(ctx, tailSet(1))
	b.Subscribe(context.Background(), tailSet(2, 3))

	for c, want := range map[int64]bool{1: true, 2: true, 3: true, 4: false} {
		if got := b.HasSubscriber(tailRow(c, "x")); got != want {
			t.Errorf("HasSubscriber(customer %d) = %v, want %v", c, got, want)
		}
	}
	cancel() // a disconnected subscriber does not count
	if b.HasSubscriber(tailRow(1, "x")) {
		t.Error("HasSubscriber counted a disconnected subscription")
	}
}
