package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

func traceListRow(at time.Time, service, traceID string) *pb.LogRow {
	return &pb.LogRow{
		Raw: "line for " + traceID, TsUnixNs: at.UnixNano(), Level: "INFO",
		Namespace: "vimgym", Service: service, Kind: "main", CustomerId: 1, TraceId: traceID,
	}
}

// setupTraceListCoordinator has trace "x" on both of two time-windowed shards,
// and 60 busier traces on the first, so x is not among the first shard's
// newest 50 although it is the newest overall.
func setupTraceListCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	var busy []*pb.LogRow
	busy = append(busy, traceListRow(t0.Add(time.Second), "api", "x"))
	for i := 0; i < 60; i++ {
		busy = append(busy, traceListRow(t0.Add(time.Duration(2+i)*time.Second), "api", fmt.Sprintf("o-%02d", i)))
	}
	a, err := shard.NewDuckDBShard("tl-a", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := shard.NewDuckDBShard("tl-b", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if err := a.Ingest(context.Background(), busy, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Ingest(context.Background(), []*pb.LogRow{traceListRow(t1.Add(time.Second), "worker", "x")}, nil); err != nil {
		t.Fatal(err)
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: testQueryStore(t, "kevlar", "vimgym", "warp")})
	addInProcessShard(t, c, &logdb.Shard{ID: "tl-a", WindowStart: t0, WindowEnd: t1, State: logdb.StateIngesting, Live: a, CreatedAt: time.Now()}, "vimgym/api")
	addInProcessShard(t, c, &logdb.Shard{ID: "tl-b", WindowStart: t1, WindowEnd: t2, State: logdb.StateIngesting, Live: b, CreatedAt: time.Now()}, "vimgym/worker")
	return c
}

type listedTrace struct {
	TraceID  string   `json:"trace_id"`
	Lines    int64    `json:"lines"`
	Services []string `json:"services"`
}

func parseListedTraces(t *testing.T, text string) []listedTrace {
	t.Helper()
	var out []listedTrace
	for _, l := range nonEmptyLines(text) {
		var lt listedTrace
		if err := json.Unmarshal([]byte(l), &lt); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, lt)
	}
	return out
}

func TestExecuteQuery_TraceListIsExactAcrossShards(t *testing.T) {
	c := setupTraceListCoordinator(t)
	spy := &spyCompute{inner: c.Compute}
	c.Compute = spy

	res, err := c.Execute(context.Background(), "trace", fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := parseListedTraces(t, drainExec(t, res).Text)

	if len(got) != 50 {
		t.Fatalf("want the newest 50 traces, got %d", len(got))
	}
	if got[0].TraceID != "x" || got[0].Lines != 2 {
		t.Errorf("x has a line on each shard and the newest of all, got %+v", got[0])
	}
	if strings.Join(got[0].Services, ",") != "vimgym/api,vimgym/worker" {
		t.Errorf("x services = %v", got[0].Services)
	}
	if n := spy.execCount.Load(); n != 2 {
		t.Fatalf("want two compute calls (ids, then stats), got %d", n)
	}
	if spy.lastReq.Aggregate != "trace_list" || spy.lastReq.TraceTag {
		t.Errorf("second call = aggregate %q tag %v", spy.lastReq.Aggregate, spy.lastReq.TraceTag)
	}
}

func TestExecuteQuery_TraceListPipesIntoStages(t *testing.T) {
	c := setupTraceListCoordinator(t)
	res, err := c.Execute(context.Background(), `trace | jq -r '.trace_id' | head -n 3`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	lines := nonEmptyLines(drainExec(t, res).Text)
	if len(lines) != 3 || lines[0] != "x" {
		t.Fatalf("got %v", lines)
	}
}

func TestExecuteQuery_TraceListScopedToANamespace(t *testing.T) {
	c := setupTraceListCoordinator(t)
	res, err := c.Execute(context.Background(), "trace vimgym/worker", fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := parseListedTraces(t, drainExec(t, res).Text)
	if len(got) != 1 || got[0].TraceID != "x" || got[0].Lines != 2 {
		t.Fatalf("only x touched vimgym/worker, and its summary covers both shards: %+v", got)
	}
}

func TestExecuteQuery_TraceListEmptyRunsOneStep(t *testing.T) {
	c := setupTestCoordinator(t) // its rows carry no trace IDs
	spy := &spyCompute{inner: c.Compute}
	c.Compute = spy
	res, err := c.Execute(context.Background(), "trace", fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if d := drainExec(t, res); d.Text != "" || len(d.Rows) != 0 {
		t.Errorf("want no output, got %+v", d)
	}
	if n := spy.execCount.Load(); n != 1 {
		t.Errorf("with no trace IDs there is nothing to summarize: want 1 compute call, got %d", n)
	}
}

// The old form `trace <id>` now reads as a namespace; say what to type.
func TestExecuteQuery_OldTraceFormHintsAtID(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(context.Background(), "trace 4bf92f3577b34da6a3ce929d0e0e4736", fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	stderr := drainExec(t, res).Stderr
	if !strings.Contains(stderr, "unknown source: 4bf92f") || !strings.Contains(stderr, "--id") {
		t.Fatalf("want an unknown-source message that mentions --id, got %q", stderr)
	}
}

// A saved cron can't be lowered for the list; it still runs through the
// ordinary two-step path and is never tagged.
func TestExecuteForWatch_TraceListRuns(t *testing.T) {
	c := setupTraceListCoordinator(t)
	cust, err := c.Store.GetOrgByName("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	w := &store.CronWatch{OrgID: cust.ID, Name: "recent-traces", Schedule: "every 1m", Pipeline: "trace", Enabled: true}
	res, err := c.ExecuteForWatch(context.Background(), w.Pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	if got := parseListedTraces(t, drainExec(t, res).Text); len(got) != 50 {
		t.Fatalf("want 50 traces, got %d", len(got))
	}
}
