package compute

import (
	"reflect"
	"strings"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// `tail -n N` on a log file gives the last N lines in chronological
// order — oldest of the tail-window first, newest last. weft's planner
// wraps each shard's `ORDER BY ts DESC LIMIT N` in an outer
// `ORDER BY ts ASC` subquery (see planner/sql.go::Query.Wrap), so
// rows arrive at compute in ts ASC. Cross-shard truncation can't use
// the head-style "stop after N emitted" trim — that would yield the
// *oldest* N of the per-shard tails. KeepLast switches the trim to
// "retain last N, emit at EOF", which gives the global last N in ASC.
//
// Test feeds two shards their ASC-ordered tail-3 windows (the wire
// shape after the subquery flip) and asserts the merged output is the
// global last 3 rows in ts ASC order.
func TestLocalComputeJob_TailMergeReturnsAscOrder(t *testing.T) {
	pool := &LocalComputePool{}
	jobIface := pool.NewJob(nil, 2, "ts", 3, true)
	job := jobIface.(*LocalComputeJob)

	// Shard A — its last 3 in ASC after the outer-flip subquery.
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 5000, Raw: "five"},
		{TsUnixNs: 7000, Raw: "seven"},
		{TsUnixNs: 9000, Raw: "nine"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")

	// Shard B — same pattern, different window.
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 6000, Raw: "six"},
		{TsUnixNs: 8000, Raw: "eight"},
		{TsUnixNs: 10000, Raw: "ten"},
	} {
		job.SendRow(r, "b")
	}
	job.SendSentinel("b")

	rows, err := job.MergedRows()
	if err != nil {
		t.Fatalf("MergedRows: %v", err)
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Raw
	}
	// Global last 3 by ts are eight (8000), nine (9000), ten (10000) —
	// in ASC display order. Anything else (e.g. DESC, or only one
	// shard's contribution) means the rolling buffer or the merge
	// regressed.
	want := []string{"eight", "nine", "ten"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tail-3 across 2 shards: got %v, want %v (ASC display order)", got, want)
	}
}

// Staged variant: `tail -n 3 ... | grep e` should still see the merged
// rows in ASC order so downstream tools (grep, awk, sed) operate on a
// log-shaped stream. Pinning this separately from the zero-stage case
// because consume() takes a different code path through warp's bash.
func TestLocalComputeJob_TailMergeAscThroughStages(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{"grep e"}, 2, "ts", 3, true)

	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 5000, Raw: "five"},
		{TsUnixNs: 7000, Raw: "seven"},
		{TsUnixNs: 9000, Raw: "nine"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 6000, Raw: "six"},
		{TsUnixNs: 8000, Raw: "eight"},
		{TsUnixNs: 10000, Raw: "ten"},
	} {
		job.SendRow(r, "b")
	}
	job.SendSentinel("b")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	// Global last 3 are eight/nine/ten. grep e matches all three. We
	// pin the order, not just the set, so a regression that flips
	// orientation but keeps the row set surfaces here.
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{"eight", "nine", "ten"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tail-3 | grep e: got %v, want %v", got, want)
	}
}

// TestLocalComputeJob_TailUnderLimit exercises the case where the
// merged stream has fewer rows than the requested N — the buffer must
// emit everything it saw without rotation (rotation logic only kicks
// in once len(buf) hits j.limit).
func TestLocalComputeJob_TailUnderLimit(t *testing.T) {
	pool := &LocalComputePool{}
	jobIface := pool.NewJob(nil, 2, "ts", 10, true)
	job := jobIface.(*LocalComputeJob)

	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 1000, Raw: "one"},
		{TsUnixNs: 3000, Raw: "three"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 2000, Raw: "two"},
	} {
		job.SendRow(r, "b")
	}
	job.SendSentinel("b")

	rows, err := job.MergedRows()
	if err != nil {
		t.Fatalf("MergedRows: %v", err)
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Raw
	}
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tail-10 over 3 rows: got %v, want %v", got, want)
	}
}
