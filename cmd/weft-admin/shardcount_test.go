package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

func TestCountRunByTime_CountsOnlyTheRunPerSecond(t *testing.T) {
	ds, err := shard.NewDuckDBShard("c", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()
	t0 := time.Date(2026, 10, 25, 21, 19, 0, 0, time.UTC)
	row := func(svc, run string, offset time.Duration) *pb.LogRow {
		return &pb.LogRow{
			Raw:        fmt.Sprintf("canary run=%s seq=1", run),
			TsUnixNs:   t0.Add(offset).UnixNano(),
			Namespace:  "roll",
			Service:    svc,
			CustomerId: 1,
		}
	}
	rows := []*pb.LogRow{
		row("roll-canary", "r1", 0),
		row("roll-canary", "r1", 300*time.Millisecond),
		row("roll-canary", "r1", 1200*time.Millisecond),
		row("roll-canary", "r2", 0),
		row("other", "r1", 0),
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got, err := countRunByTime(context.Background(), ds, "roll-canary", "r1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	want := []secondCount{{t0, 2}, {t0.Add(time.Second), 1}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) || got[i].N != want[i].N {
			t.Errorf("bucket %d = %v, want %v", i, got[i], want[i])
		}
	}
}
