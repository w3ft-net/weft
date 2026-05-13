package compute

import (
	"context"
	"reflect"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// drainOnlineMerge feeds the script as ComputeMessages into onlineMerge
// and returns the rows emitted in order. shardCount is passed through;
// limit caps the emit count when > 0. All test scripts deliver enough
// sentinels (== shardCount) so onlineMerge returns.
func drainOnlineMerge(t *testing.T, script []ComputeMessage, shardCount int, mergeKey string, limit int) []*pb.ResultRow {
	t.Helper()
	ch := make(chan ComputeMessage, len(script))
	for _, msg := range script {
		ch <- msg
	}
	var got []*pb.ResultRow
	onlineMerge(ch, shardCount, mergeKey, func(row *pb.ResultRow) bool {
		if limit > 0 && len(got) >= limit {
			return false
		}
		got = append(got, row)
		return true
	})
	return got
}

func rawsOf(rows []*pb.ResultRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Raw
	}
	return out
}

func TestOnlineMerge_TsAsc_FullyOrdered(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(1, "one"), ShardID: "a"},
		{Row: row(3, "three"), ShardID: "a"},
		{Row: row(5, "five"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{Row: row(2, "two"), ShardID: "b"},
		{Row: row(4, "four"), ShardID: "b"},
		{Row: row(6, "six"), ShardID: "b"},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "ts", 0))
	want := []string{"one", "two", "three", "four", "five", "six"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge order = %v, want %v", got, want)
	}
}

// TestOnlineMerge_OneShardLeadsTheOther — A's rows arrive first, then
// B's. Because mergeRowsFull would simply pre-sort and emit, but the
// online merge has to make decisions row-by-row, this exercises the
// "block until B is heard from" path.
func TestOnlineMerge_OneShardLeadsTheOther(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(1, "one"), ShardID: "a"},
		{Row: row(2, "two"), ShardID: "a"},
		{Row: row(3, "three"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{Row: row(4, "four"), ShardID: "b"},
		{Row: row(5, "five"), ShardID: "b"},
		{Row: row(6, "six"), ShardID: "b"},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "ts", 0))
	want := []string{"one", "two", "three", "four", "five", "six"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge order = %v, want %v", got, want)
	}
}

// TestOnlineMerge_ShardSentinelMidstream — A's sentinel arrives before
// all of B's rows. After A is sentinel'd the merge must drain B alone.
func TestOnlineMerge_ShardSentinelMidstream(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(1, "one"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{Row: row(2, "two"), ShardID: "b"},
		{Row: row(3, "three"), ShardID: "b"},
		{Row: row(4, "four"), ShardID: "b"},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "ts", 0))
	want := []string{"one", "two", "three", "four"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge order = %v, want %v", got, want)
	}
}

// TestOnlineMerge_EmptyShardSentinelOnly — B sends only a sentinel. A
// drains alone.
func TestOnlineMerge_EmptyShardSentinelOnly(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(1, "one"), ShardID: "a"},
		{Row: row(2, "two"), ShardID: "a"},
		{Row: row(3, "three"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "ts", 0))
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge order = %v, want %v", got, want)
	}
}

func TestOnlineMerge_TsDesc(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(5, "five"), ShardID: "a"},
		{Row: row(3, "three"), ShardID: "a"},
		{Row: row(1, "one"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{Row: row(6, "six"), ShardID: "b"},
		{Row: row(4, "four"), ShardID: "b"},
		{Row: row(2, "two"), ShardID: "b"},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "ts DESC", 0))
	want := []string{"six", "five", "four", "three", "two", "one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge order = %v, want %v", got, want)
	}
}

func TestOnlineMerge_RawAsc(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(0, "alpha"), ShardID: "a"},
		{Row: row(0, "cherry"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{Row: row(0, "bravo"), ShardID: "b"},
		{Row: row(0, "delta"), ShardID: "b"},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "raw", 0))
	want := []string{"alpha", "bravo", "cherry", "delta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge order = %v, want %v", got, want)
	}
}

// TestOnlineMerge_ConcatModeArrivalOrder — empty mergeKey emits in
// arrival order, regardless of any apparent column order. Used by the
// planner's Mode C path; downstream stages re-impose order.
func TestOnlineMerge_ConcatModeArrivalOrder(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(99, "first-arrived"), ShardID: "a"},
		{Row: row(1, "second-arrived"), ShardID: "b"},
		{Row: row(50, "third-arrived"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "", 0))
	want := []string{"first-arrived", "second-arrived", "third-arrived"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("concat order = %v, want %v", got, want)
	}
}

// TestOnlineMerge_GlobalLimit — emit returns false past the cap, but
// the merge keeps draining the channel so producers don't deadlock.
func TestOnlineMerge_GlobalLimit(t *testing.T) {
	script := []ComputeMessage{
		{Row: row(1, "one"), ShardID: "a"},
		{Row: row(3, "three"), ShardID: "a"},
		{Row: row(5, "five"), ShardID: "a"},
		{ShardID: "a", Sentinel: true},
		{Row: row(2, "two"), ShardID: "b"},
		{Row: row(4, "four"), ShardID: "b"},
		{Row: row(6, "six"), ShardID: "b"},
		{ShardID: "b", Sentinel: true},
	}
	got := rawsOf(drainOnlineMerge(t, script, 2, "ts", 3))
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("limited merge = %v, want %v", got, want)
	}
}

// TestOnlineMerge_StreamingProperty pins the central streaming
// invariant: a row emerges from the merge BEFORE every shard has sent
// its sentinel. Without the online merge we can only emit after all
// shards complete; with it, A's row 1 fires as soon as B sends ANY
// row (because B's row 2 lets the merge prove 1 < 2 < everything-B).
func TestOnlineMerge_StreamingProperty(t *testing.T) {
	ch := make(chan ComputeMessage)
	emitted := make(chan *pb.ResultRow)
	done := make(chan struct{})

	go func() {
		defer close(done)
		onlineMerge(ch, 2, "ts", func(r *pb.ResultRow) bool {
			emitted <- r
			return true
		})
	}()

	// A sends row 1. Cannot emit yet — B hasn't been heard from.
	ch <- ComputeMessage{Row: row(1, "one"), ShardID: "a"}
	select {
	case got := <-emitted:
		t.Fatalf("emitted %q before shard b reported", got.Raw)
	case <-time.After(50 * time.Millisecond):
	}

	// B sends row 2. Now A's 1 can be emitted (1 < 2, both heads known).
	ch <- ComputeMessage{Row: row(2, "two"), ShardID: "b"}
	select {
	case got := <-emitted:
		if got.Raw != "one" {
			t.Fatalf("first emit = %q, want %q", got.Raw, "one")
		}
	case <-time.After(time.Second):
		t.Fatal("merge did not emit after b sent a row")
	}

	// A's queue is now empty and not sentinel'd → blocked again. B's
	// 2 cannot emit yet (A might still send 1.5). Send A's sentinel:
	// now A is exhausted and B's 2 is the next emit.
	ch <- ComputeMessage{ShardID: "a", Sentinel: true}
	select {
	case got := <-emitted:
		if got.Raw != "two" {
			t.Fatalf("second emit = %q, want %q", got.Raw, "two")
		}
	case <-time.After(time.Second):
		t.Fatal("merge did not emit after a sent sentinel")
	}

	ch <- ComputeMessage{ShardID: "b", Sentinel: true}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onlineMerge did not return after final sentinel")
	}
}

// TestOnlineMerge_ZeroShardsExitsImmediately — degenerate case: no
// shards at all (the coordinator returns an empty stream when no
// shards match the time range). onlineMerge must not block.
func TestOnlineMerge_ZeroShardsExitsImmediately(t *testing.T) {
	ch := make(chan ComputeMessage)
	done := make(chan struct{})
	go func() {
		defer close(done)
		onlineMerge(ch, 0, "ts", func(*pb.ResultRow) bool { return true })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onlineMerge did not return for shardCount=0")
	}
}

// TestOnlineMerge_OneShard — single shard, no merge math required.
// Each row should emit as soon as it arrives.
func TestOnlineMerge_OneShard(t *testing.T) {
	ch := make(chan ComputeMessage)
	emitted := make(chan *pb.ResultRow)
	done := make(chan struct{})
	go func() {
		defer close(done)
		onlineMerge(ch, 1, "ts", func(r *pb.ResultRow) bool {
			emitted <- r
			return true
		})
	}()
	ch <- ComputeMessage{Row: row(1, "one"), ShardID: "a"}
	select {
	case got := <-emitted:
		if got.Raw != "one" {
			t.Fatalf("got %q, want %q", got.Raw, "one")
		}
	case <-time.After(time.Second):
		t.Fatal("single-shard merge did not emit immediately")
	}
	ch <- ComputeMessage{ShardID: "a", Sentinel: true}
	<-done
}

// TestLocalComputeJob_OnlineStreaming asserts the consume() side of
// the streaming property: Stream.Next returns A's first row before
// every shard has sent its sentinel. This pins that consume()'s
// refactor to onlineMerge actually surfaces the stream end-to-end.
func TestLocalComputeJob_OnlineStreaming(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 2, "ts", 0, false)

	job.SendRow(row(1, "one"), "a")
	job.SendRow(row(2, "two"), "b")
	// Shard A has not yet sent its sentinel; shard B has not yet sent
	// its sentinel. A 1 < B 2 already determines the next emit.

	stream := job.Stream()
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	line, ok, err := stream.Next(ctx)
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if line.Raw != "one" {
		t.Fatalf("first stream line = %q, want %q (must emit before all sentinels)", line.Raw, "one")
	}

	job.SendSentinel("a")
	job.SendSentinel("b")

	line, ok, err = stream.Next(ctx)
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if line.Raw != "two" {
		t.Fatalf("second stream line = %q, want %q", line.Raw, "two")
	}
	if _, ok, err := stream.Next(ctx); ok || err != nil {
		t.Fatalf("expected end-of-stream, got ok=%v err=%v", ok, err)
	}
}
