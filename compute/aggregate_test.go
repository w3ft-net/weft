package compute

import (
	"context"
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestAggregate_UnknownFailsTheQuery: an engine that doesn't know an
// aggregate would print the shards' distinct lines as if they were the
// rows, so it refuses the query instead.
func TestAggregate_UnknownFailsTheQuery(t *testing.T) {
	_, err := (&Engine{}).ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Aggregate: "median"})
	if err == nil || !strings.Contains(err.Error(), `unknown aggregate "median"`) {
		t.Fatalf("err = %v, want unknown aggregate", err)
	}
}

// TestAggregate_RowWithoutCountFails: a shard row missing its count
// is an error, not a count of 0.
func TestAggregate_RowWithoutCountFails(t *testing.T) {
	job := (&Engine{}).NewJob(nil, 1, "raw", 0, false, false)
	job.aggregate = aggCountDistinct
	go func() {
		job.SendRow(&pb.ResultRow{Raw: "a"}, "s")
		job.SendSentinel("s")
	}()
	if _, err := job.Result(); err == nil || !strings.Contains(err.Error(), "without a count") {
		t.Fatalf("err = %v, want a missing-count error", err)
	}
}
