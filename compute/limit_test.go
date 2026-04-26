package compute

import (
	"strings"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestLocalComputeJob_LimitTruncatesMergedStream is the regression
// test for the tail-N-per-shard amplification bug:
//
//	WEFT_ENDPOINT=http://localhost:8080 weft sh -c \
//	  "tail -n 500 /log/acme/api.log | wc -l"
//	# expected: ≤ 500
//	# actual (before fix): ≈ 500 × num_shards (per-shard LIMIT was the
//	# only one applied; compute saw the full union and counted it)
//
// The compute pool must truncate the merged stream to the planner's
// global limit before the stages run, otherwise downstream stages
// (`wc -l`, `head`, `awk`) see the unmerged amplification.
func TestLocalComputeJob_LimitTruncatesMergedStream(t *testing.T) {
	pool := &LocalComputePool{}
	// Two shards, 5 rows each, line_num-interleaved so the merge
	// produces a deterministic 1..10 sequence.
	job := pool.NewJob([]string{"wc -l"}, 2, "line_num", 3)
	for _, r := range []*pb.ResultRow{
		{LineNum: 1, Raw: "one"},
		{LineNum: 3, Raw: "three"},
		{LineNum: 5, Raw: "five"},
		{LineNum: 7, Raw: "seven"},
		{LineNum: 9, Raw: "nine"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	for _, r := range []*pb.ResultRow{
		{LineNum: 2, Raw: "two"},
		{LineNum: 4, Raw: "four"},
		{LineNum: 6, Raw: "six"},
		{LineNum: 8, Raw: "eight"},
		{LineNum: 10, Raw: "ten"},
	} {
		job.SendRow(r, "b")
	}
	job.SendSentinel("b")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	got := strings.TrimSpace(out)
	if got != "3" {
		t.Errorf("wc -l with limit=3 over 10 merged rows: got %q, want %q", got, "3")
	}
}

// TestLocalComputeJob_LimitZeroIsUnlimited verifies the existing
// "no limit" code path still passes the full merged stream through.
// Catches a future refactor where Limit becomes a required field
// and zero accidentally truncates everything.
func TestLocalComputeJob_LimitZeroIsUnlimited(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{"wc -l"}, 2, "line_num", 0)
	for _, r := range []*pb.ResultRow{
		{LineNum: 1, Raw: "one"},
		{LineNum: 3, Raw: "three"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	for _, r := range []*pb.ResultRow{
		{LineNum: 2, Raw: "two"},
		{LineNum: 4, Raw: "four"},
	} {
		job.SendRow(r, "b")
	}
	job.SendSentinel("b")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	got := strings.TrimSpace(out)
	if got != "4" {
		t.Errorf("wc -l with limit=0 over 4 rows: got %q, want %q", got, "4")
	}
}
