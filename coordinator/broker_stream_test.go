package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func TestBrokerStream_RowsThroughFilter(t *testing.T) {
	ch := make(chan *pb.LogRow, 4)
	bs := &brokerStream{
		ch:     ch,
		filter: passthroughFilter,
	}

	ch <- &pb.LogRow{Raw: "first"}
	ch <- &pb.LogRow{Raw: "second"}
	close(ch)

	ctx := context.Background()
	got := drainBrokerRaws(t, ctx, bs)
	want := []string{"first", "second"}
	if !equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

func TestBrokerStream_FilterReturnsZero_RowSkipped(t *testing.T) {
	ch := make(chan *pb.LogRow, 4)
	rejectAll := func(*pb.LogRow) []string { return nil }
	bs := &brokerStream{ch: ch, filter: rejectAll}

	ch <- &pb.LogRow{Raw: "rejected"}
	close(ch)

	ctx := context.Background()
	_, ok, err := bs.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ok {
		t.Error("expected EOF after all rows rejected, got line")
	}
}

func TestBrokerStream_MultiLineFilter_QueuesExtras(t *testing.T) {
	ch := make(chan *pb.LogRow, 1)
	expand := func(row *pb.LogRow) []string {
		return []string{row.Raw + "-a", row.Raw + "-b", row.Raw + "-c"}
	}
	bs := &brokerStream{ch: ch, filter: expand}

	ch <- &pb.LogRow{Raw: "row"}
	close(ch)

	ctx := context.Background()
	got := drainBrokerRaws(t, ctx, bs)
	want := []string{"row-a", "row-b", "row-c"}
	if !equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

func TestBrokerStream_CtxCancel_Returns(t *testing.T) {
	ch := make(chan *pb.LogRow)
	bs := &brokerStream{ch: ch, filter: passthroughFilter}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _, err := bs.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestBrokerStream_Close_Unsubscribes(t *testing.T) {
	calls := 0
	bs := &brokerStream{
		ch:     make(chan *pb.LogRow),
		filter: passthroughFilter,
		unsub:  func() { calls++ },
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close (2nd): %v", err)
	}
	if calls != 1 {
		t.Errorf("unsub called %d times, want 1", calls)
	}
}

func passthroughFilter(row *pb.LogRow) []string {
	return []string{row.Raw}
}

func drainBrokerRaws(t *testing.T, ctx context.Context, bs *brokerStream) []string {
	t.Helper()
	var raws []string
	for {
		line, ok, err := bs.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return raws
		}
		raws = append(raws, line.Raw)
	}
}
