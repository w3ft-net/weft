package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/shard"
)

func traceLine(i int, service, traceID, level, spanName, parent string) *pb.LogRow {
	return &pb.LogRow{
		TsUnixNs: spanBase.Add(time.Duration(i) * time.Millisecond).UnixNano(),
		Raw:      fmt.Sprintf("line %d", i), Namespace: "acme", Service: service, Kind: "main", Level: level, CustomerId: 1,
		TraceId: traceID, SpanName: spanName, SpanId: fmt.Sprintf("sp%d", i), ParentSpanId: parent,
	}
}

// runStep runs one shard SQL over every shard and merges it as the compute
// engine does.
func runStep(t *testing.T, sql string, args []any, aggregate string, shards ...*shard.DuckDBShard) []string {
	t.Helper()
	job := (&Engine{}).NewJob(nil, len(shards), "raw", 0, false, false)
	job.aggregate = aggregate
	for i, ds := range shards {
		rows, err := ds.Query(context.Background(), sql, args...)
		if err != nil {
			t.Fatalf("shard %d: %v\n%s", i, err, sql)
		}
		name := fmt.Sprintf("shard-%d", i)
		for _, r := range rows {
			job.SendRow(r, name)
		}
		job.SendSentinel(name)
	}
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(out), "\n")
}

type traceSummary struct {
	TraceID  string   `json:"trace_id"`
	StartNs  int64    `json:"start_ns"`
	EndNs    int64    `json:"end_ns"`
	DurNs    int64    `json:"dur_ns"`
	Lines    int64    `json:"lines"`
	Errors   *int64   `json:"errors"`
	Services []string `json:"services"`
	Root     string   `json:"root"`
}

// runTraceList does what the coordinator does for a bare `trace`: find the
// newest trace IDs, then summarize exactly those.
func runTraceList(t *testing.T, pipeline string, shards ...*shard.DuckDBShard) []traceSummary {
	t.Helper()
	plan, err := planner.Plan(pipeline, shapetest.Group(1), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, l := range runStep(t, plan.DirectShardSQL, plan.Args, plan.Aggregate, shards...) {
		var r struct {
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("step 1 line %q: %v", l, err)
		}
		ids = append(ids, r.TraceID)
	}
	var out []traceSummary
	for _, l := range runStep(t, plan.TraceStats.SQL, plan.TraceStats.WithIDs(ids), plan.TraceStats.Aggregate, shards...) {
		var s traceSummary
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatalf("step 2 line %q: %v", l, err)
		}
		out = append(out, s)
	}
	return out
}

func TestTraceList_NewestFirstWithWholeTraceStats(t *testing.T) {
	api := newSpanShard(t, "tl-api", []*pb.LogRow{
		traceLine(1, "api", "t1", "INFO", "GET /a", ""),
		traceLine(4, "api", "t1", "INFO", "", ""),
		traceLine(7, "api", "t2", "ERROR", "", ""),
	})
	worker := newSpanShard(t, "tl-worker", []*pb.LogRow{
		traceLine(2, "worker", "t1", "ERROR", "job.run", "sp1"),
		traceLine(3, "worker", "t1", "INFO", "", ""),
	})
	got := runTraceList(t, "trace", api, worker)
	if len(got) != 2 || got[0].TraceID != "t2" || got[1].TraceID != "t1" {
		t.Fatalf("want t2 then t1 (newest first), got %+v", got)
	}
	t1 := got[1]
	if t1.Lines != 4 {
		t.Errorf("t1 has 4 lines across both shards, got %d", t1.Lines)
	}
	if t1.Errors == nil || *t1.Errors != 1 {
		t.Errorf("t1 has 1 ERROR line, got %v", t1.Errors)
	}
	if strings.Join(t1.Services, ",") != "acme/api,acme/worker" {
		t.Errorf("t1 services = %v", t1.Services)
	}
	if t1.DurNs != 3*int64(time.Millisecond) || t1.EndNs-t1.StartNs != t1.DurNs {
		t.Errorf("t1 spans lines 1ms..4ms, got start=%d end=%d dur=%d", t1.StartNs, t1.EndNs, t1.DurNs)
	}
	if t1.Root != "GET /a" {
		t.Errorf("t1 root span = %q, want the parentless span", t1.Root)
	}
}

// A trace can be among the newest on one shard and not on another. The stats
// still have to cover all its lines, which is why the list takes two steps.
func TestTraceList_ExactWhenATraceIsNotNewestOnEveryShard(t *testing.T) {
	var busy []*pb.LogRow
	busy = append(busy, traceLine(0, "api", "x", "INFO", "", ""))
	for i := 1; i <= 60; i++ {
		busy = append(busy, traceLine(i, "api", fmt.Sprintf("other-%02d", i), "INFO", "", ""))
	}
	a := newSpanShard(t, "tl-busy", busy)
	b := newSpanShard(t, "tl-quiet", []*pb.LogRow{traceLine(100, "worker", "x", "INFO", "", "")})

	got := runTraceList(t, "trace", a, b)
	if len(got) != planner.TraceListLimit {
		t.Fatalf("want %d traces, got %d", planner.TraceListLimit, len(got))
	}
	if got[0].TraceID != "x" {
		t.Fatalf("x has the newest line (100ms), want it first: %+v", got[0])
	}
	if got[0].Lines != 2 {
		t.Errorf("x has a line on each shard, so 2; got %d (a shard's newest-50 cut leaked into the stats)", got[0].Lines)
	}
	if strings.Join(got[0].Services, ",") != "acme/api,acme/worker" {
		t.Errorf("x services = %v", got[0].Services)
	}
}

func TestTraceList_ErrorsUnknownWithoutLevels(t *testing.T) {
	ds := newSpanShard(t, "tl-nolevel", []*pb.LogRow{
		traceLine(1, "legacy", "no-level", "", "", ""),
		traceLine(2, "api", "has-level", "INFO", "", ""),
	})
	got := runTraceList(t, "trace", ds)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	byID := map[string]traceSummary{got[0].TraceID: got[0], got[1].TraceID: got[1]}
	if byID["no-level"].Errors != nil {
		t.Errorf("no line has a level, so errors is unknown (null), got %d", *byID["no-level"].Errors)
	}
	if e := byID["has-level"].Errors; e == nil || *e != 0 {
		t.Errorf("levels recorded and none errored, want 0, got %v", e)
	}
}

func TestTraceList_ScopePicksTracesButSummarizesWholeTrace(t *testing.T) {
	ds := newSpanShard(t, "tl-scope", []*pb.LogRow{
		traceLine(1, "api", "touches-api", "INFO", "", ""),
		traceLine(2, "worker", "touches-api", "INFO", "", ""),
		traceLine(3, "worker", "worker-only", "INFO", "", ""),
	})
	got := runTraceList(t, "trace acme/api", ds)
	if len(got) != 1 || got[0].TraceID != "touches-api" {
		t.Fatalf("want only the trace that touched acme/api, got %+v", got)
	}
	if got[0].Lines != 2 || strings.Join(got[0].Services, ",") != "acme/api,acme/worker" {
		t.Errorf("the summary covers the whole trace: %+v", got[0])
	}
}

func TestTraceList_IgnoresLinesWithoutATraceID(t *testing.T) {
	ds := newSpanShard(t, "tl-notrace", []*pb.LogRow{
		traceLine(1, "api", "", "INFO", "", ""),
		traceLine(2, "api", "t1", "INFO", "", ""),
	})
	got := runTraceList(t, "trace", ds)
	if len(got) != 1 || got[0].TraceID != "t1" {
		t.Fatalf("got %+v", got)
	}
}

func TestTraceList_EmptyWhenNothingHasATrace(t *testing.T) {
	ds := newSpanShard(t, "tl-empty", []*pb.LogRow{traceLine(1, "api", "", "INFO", "", "")})
	if got := runTraceList(t, "trace", ds); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}
