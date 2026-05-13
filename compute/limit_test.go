package compute

import (
	"strings"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func TestLocalComputeJob_LimitTruncatesMergedStream(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{"wc -l"}, 2, "ts", 3, false)
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 1000, Raw: "one"},
		{TsUnixNs: 3000, Raw: "three"},
		{TsUnixNs: 5000, Raw: "five"},
		{TsUnixNs: 7000, Raw: "seven"},
		{TsUnixNs: 9000, Raw: "nine"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 2000, Raw: "two"},
		{TsUnixNs: 4000, Raw: "four"},
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
	got := strings.TrimSpace(out)
	if got != "3" {
		t.Errorf("wc -l with limit=3 over 10 merged rows: got %q, want %q", got, "3")
	}
}

func TestLocalComputeJob_LimitZeroIsUnlimited(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{"wc -l"}, 2, "ts", 0, false)
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 1000, Raw: "one"},
		{TsUnixNs: 3000, Raw: "three"},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 2000, Raw: "two"},
		{TsUnixNs: 4000, Raw: "four"},
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
