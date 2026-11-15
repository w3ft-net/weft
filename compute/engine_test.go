package compute

import (
	"context"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

func TestZeroStageJob_LineNumIsSequential(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob(nil, 2, "ts", 0, false, false)

	go func() {
		job.SendRow(&pb.ResultRow{LineNum: 100, Raw: "first", TsUnixNs: 1000}, "a")
		job.SendRow(&pb.ResultRow{LineNum: 300, Raw: "third", TsUnixNs: 3000}, "a")
		job.SendSentinel("a")
		job.SendRow(&pb.ResultRow{LineNum: 200, Raw: "second", TsUnixNs: 2000}, "b")
		job.SendRow(&pb.ResultRow{LineNum: 400, Raw: "fourth", TsUnixNs: 4000}, "b")
		job.SendSentinel("b")
	}()

	stream := job.Stream()
	defer stream.Close()

	ctx := context.Background()
	var gotNums []int64
	for {
		line, ok, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		gotNums = append(gotNums, line.LineNum)
	}

	want := []int64{1, 2, 3, 4}
	if len(gotNums) != len(want) {
		t.Fatalf("got %d lines, want %d", len(gotNums), len(want))
	}
	for i := range want {
		if gotNums[i] != want[i] {
			t.Errorf("Line[%d].LineNum = %d, want %d", i, gotNums[i], want[i])
		}
	}
}
