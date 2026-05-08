package compute

import (
	"context"
	"reflect"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func TestLocalComputeJob_Stream(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 2, "ts", 0)

	go func() {
		job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "one"}, "a")
		job.SendRow(&pb.ResultRow{TsUnixNs: 3000, Raw: "three"}, "a")
		job.SendSentinel("a")
		job.SendRow(&pb.ResultRow{TsUnixNs: 2000, Raw: "two"}, "b")
		job.SendRow(&pb.ResultRow{TsUnixNs: 4000, Raw: "four"}, "b")
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
