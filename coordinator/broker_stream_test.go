package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestBrokerStream_RowsThroughFilter feeds two rows into the channel
// and asserts the LiveFilter projects each row's Raw onto the stream.
// passthrough filter is what BuildLiveFilter returns for stage-less
// pipelines.
func TestBrokerStream_RowsThroughFilter(t *testing.T) {
	ch := make(chan *pb.LogRow, 4)
	bs := &brokerStream{
		ch:     ch,
		filter: passthroughFilter,
	}

	ch <- &pb.LogRow{LineNum: 1, Raw: "first"}
	ch <- &pb.LogRow{LineNum: 2, Raw: "second"}
	close(ch)

	ctx := context.Background()
	got, _ := drainBroker(t, ctx, bs)
	want := []string{"first", "second"}
	if !equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

// TestBrokerStream_FilterReturnsZero_RowSkipped covers the empty-
// filter-output path: rows whose filter() returns nil are dropped
// silently and the stream waits for the next channel send.
func TestBrokerStream_FilterReturnsZero_RowSkipped(t *testing.T) {
	ch := make(chan *pb.LogRow, 4)
	rejectAll := func(*pb.LogRow) []string { return nil }
	bs := &brokerStream{ch: ch, filter: rejectAll}

	ch <- &pb.LogRow{LineNum: 1, Raw: "rejected"}
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

// TestBrokerStream_MultiLineFilter_QueuesExtras verifies that when a
// filter returns several lines for a single row, brokerStream emits
// them across consecutive Next calls without losing any.
func TestBrokerStream_MultiLineFilter_QueuesExtras(t *testing.T) {
	ch := make(chan *pb.LogRow, 1)
	expand := func(row *pb.LogRow) []string {
		return []string{row.Raw + "-a", row.Raw + "-b", row.Raw + "-c"}
	}
	bs := &brokerStream{ch: ch, filter: expand}

	ch <- &pb.LogRow{LineNum: 5, Raw: "row"}
	close(ch)

	ctx := context.Background()
	got, lineNums := drainBroker(t, ctx, bs)
	want := []string{"row-a", "row-b", "row-c"}
	if !equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	for _, ln := range lineNums {
		if ln != 5 {
			t.Errorf("LineNum = %d, want 5 on every emitted line", ln)
		}
	}
}

// TestBrokerStream_CtxCancel_Returns: pin the cancellation contract.
// A blocked Next returns ctx.Err() promptly when the context cancels —
// without that, follow streams would leak goroutines on client
// disconnect.
func TestBrokerStream_CtxCancel_Returns(t *testing.T) {
	ch := make(chan *pb.LogRow) // unbuffered + never sent on → blocks
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

// TestBrokerStream_Close_Unsubscribes pins the cleanup contract.
// Close runs the unsub callback exactly once even on repeated calls.
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

func drainBroker(t *testing.T, ctx context.Context, bs *brokerStream) ([]string, []int64) {
	t.Helper()
	var raws []string
	var nums []int64
	for {
		line, ok, err := bs.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return raws, nums
		}
		raws = append(raws, line.Raw)
		nums = append(nums, line.LineNum)
	}
}
