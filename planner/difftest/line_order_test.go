package difftest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/ingest"
	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// TestLineOrderAcrossShards: multi-line events that tie on ts, on
// different shards, come out with each event's lines together and in
// order, one-line events first, for cat/head/tail alike. An event's lines live on one
// shard, so rows are placed per event, not round-robin.
func TestLineOrderAcrossShards(t *testing.T) {
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	row := func(raw string, at int) *pb.LogRow {
		return &pb.LogRow{Raw: raw, TsUnixNs: base.Add(time.Duration(at) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1}
	}
	// Mint ids in a fixed order so event_id order is known: A < B < E.
	a := ingest.SplitLines([]*pb.LogRow{row("a1\na2\na3", 0)})
	b := ingest.SplitLines([]*pb.LogRow{row("b1\nb2", 0)})
	e := ingest.SplitLines([]*pb.LogRow{row("e1\ne2", 1)})
	d := []*pb.LogRow{row("d", 0)}
	c := []*pb.LogRow{row("c", 1)}

	h := newHarness(t, nil, 2)
	// Interleave the events across shards so every ts tie spans both.
	for sh, rows := range [][]*pb.LogRow{append(append(append([]*pb.LogRow{}, a...), e...), c...), append(append([]*pb.LogRow{}, b...), d...)} {
		if err := h.shards[sh].Ingest(context.Background(), rows, nil); err != nil {
			t.Fatal(err)
		}
	}

	// The order the plan promises: (ts, event_id, line_no), no event first.
	all := append(append(append(append(append([]*pb.LogRow{}, a...), b...), e...), d...), c...)
	sort.SliceStable(all, func(i, j int) bool {
		x, y := all[i], all[j]
		if x.TsUnixNs != y.TsUnixNs {
			return x.TsUnixNs < y.TsUnixNs
		}
		if x.EventId != y.EventId {
			return x.EventId < y.EventId
		}
		return x.LineNo < y.LineNo
	})
	var order []string
	for _, r := range all {
		order = append(order, r.Raw)
	}
	if got, want := strings.Join(order, " "), "d a1 a2 a3 b1 b2 c e1 e2"; got != want {
		t.Fatalf("expected order is %q, want %q", got, want)
	}

	for _, tc := range []struct {
		pipeline string
		want     []string
	}{
		{`cat /log/acme/api/main.log`, order},
		{`cat /log/acme/api/main.log | head -n 5`, order[:5]},
		{`cat /log/acme/api/main.log | head -n 3`, order[:3]},
		// tail: the shards' DESC LIMIT, then the ASC re-sort.
		{`cat /log/acme/api/main.log | tail -n 4`, order[len(order)-4:]},
		{`cat /log/acme/api/main.log | tail -n 6`, order[len(order)-6:]},
		{`cat /log/acme/api/main.log | grep -v '^d$'`, order[1:]},
	} {
		t.Run(tc.pipeline, func(t *testing.T) {
			plan, err := planner.Plan(tc.pipeline, shapetest.Group(1), "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.runPlan(plan)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Join(tc.want, "\n"); strings.TrimSuffix(got, "\n") != want {
				t.Errorf("%s\n got: %q\nwant: %q", tc.pipeline, got, fmt.Sprint(want))
			}
		})
	}
}
