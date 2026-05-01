package compute

import (
	"context"
	"reflect"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestLocalComputeJob_Stream pins the new line-streaming surface on
// ComputeJob. Today merge output is exposed only via Result() as a
// single concatenated string; Stream() is the forward-only line-by-line
// surface that downstream callers (coordinator → gRPC server) drain
// incrementally. The shape mirrors what bash pipelines actually emit:
// a sequence of rows, not a blob.
func TestLocalComputeJob_Stream(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 2, "line_num", 0)

	// Two shards interleave by line_num. The merge must emit lines in
	// line_num order: 1, 2, 3, 4. Sentinels arrive after each shard's
	// rows so the merge has work to do.
	go func() {
		job.SendRow(&pb.ResultRow{LineNum: 1, Raw: "one"}, "a")
		job.SendRow(&pb.ResultRow{LineNum: 3, Raw: "three"}, "a")
		job.SendSentinel("a")
		job.SendRow(&pb.ResultRow{LineNum: 2, Raw: "two"}, "b")
		job.SendRow(&pb.ResultRow{LineNum: 4, Raw: "four"}, "b")
		job.SendSentinel("b")
	}()

	stream := job.Stream()
	defer stream.Close()

	ctx := context.Background()
	var got []string
	for {
		line, ok, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, line.Raw)
	}

	want := []string{"one", "two", "three", "four"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stream lines = %v, want %v", got, want)
	}
}
