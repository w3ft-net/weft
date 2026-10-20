package compute

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/theme"
)

var spanBase = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func sdkSpanRow(i int, service, name, id, parent, level string, dur int64, body string) *pb.LogRow {
	return &pb.LogRow{
		TsUnixNs: spanBase.Add(time.Duration(i) * time.Millisecond).UnixNano(),
		Raw:      body, Namespace: "acme", Service: service, Kind: "main", Level: level,
		TraceId: "t1", SpanName: name, SpanId: id, ParentSpanId: parent, DurationNs: dur,
	}
}

func doneBody() string  { return `{"msg":"span done","status":"ok"}` }
func errorBody() string { return `{"msg":"span error","err":"boom"}` }

func newSpanShard(t *testing.T, id string, rows []*pb.LogRow) *shard.DuckDBShard {
	t.Helper()
	ds, err := shard.NewDuckDBShard(id, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	return ds
}

func runSpanQuery(t *testing.T, pipeline string, shards ...*shard.DuckDBShard) []string {
	t.Helper()
	plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	job := (&Engine{}).NewJob(plan.ComputeStages, len(shards), plan.ShardOrderBy, 0, false, false)
	job.aggregate = plan.Aggregate
	for i, ds := range shards {
		rows, err := ds.Query(context.Background(), plan.ShardSQL, plan.Args...)
		if err != nil {
			t.Fatalf("shard %d: %v\n%s", i, err, plan.ShardSQL)
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
	return strings.Split(strings.TrimSpace(out), "\n")
}

func TestSDKWrittenSpansRollUp(t *testing.T) {
	a := newSpanShard(t, "span-a", []*pb.LogRow{
		sdkSpanRow(0, "api", "db.query", "s2", "s1", "INFO", 0, `{"msg":"span start"}`),
		sdkSpanRow(5, "api", "db.query", "s2", "s1", "INFO", 5_000_000, doneBody()),
		sdkSpanRow(9, "api", "api.handle", "s1", "", "INFO", 9_000_000, doneBody()),
		sdkSpanRow(7, "api", "db.query", "s3", "s1", "ERROR", 0, errorBody()),
	})
	b := newSpanShard(t, "span-b", []*pb.LogRow{
		sdkSpanRow(6, "worker", "db.query", "s4", "s1", "INFO", 15_000_000, doneBody()),
	})

	rows := theme.RenderSpanAggregate(runSpanQuery(t, "trace spans", a, b))
	if len(rows) != 2 {
		t.Fatalf("rolled up %d span names from SDK-written rows, want 2 (api.handle, db.query): %+v", len(rows), rows)
	}
	db := rows[1]
	if db.Name != "db.query" || db.Count() != 2 || db.Errors != 1 {
		t.Fatalf("db.query = %+v, want 2 calls and 1 error", db)
	}
	if db.Total != 20*time.Millisecond || db.Avg != 10*time.Millisecond || db.Min != 5*time.Millisecond || db.Max != 15*time.Millisecond {
		t.Errorf("exact stats wrong: %+v", db)
	}
	if db.P99 < 14*time.Millisecond || db.P99 > 15*time.Millisecond {
		t.Errorf("p99 = %v, want within 1%% of 15ms", db.P99)
	}
	if db.P50 < 4950*time.Microsecond || db.P50 > 5050*time.Microsecond {
		t.Errorf("p50 = %v, want within 1%% of 5ms", db.P50)
	}
}

func TestSpansByService(t *testing.T) {
	a := newSpanShard(t, "span-svc-a", []*pb.LogRow{
		sdkSpanRow(5, "api", "db.query", "s2", "", "INFO", 5_000_000, doneBody()),
		sdkSpanRow(6, "worker", "db.query", "s3", "", "INFO", 15_000_000, doneBody()),
	})
	rows := theme.RenderSpanAggregate(runSpanQuery(t, "trace spans --by service", a))
	if len(rows) != 2 || rows[0].Label != "acme/api" || rows[1].Label != "acme/worker" {
		t.Fatalf("rows = %+v, want one per service", rows)
	}
}

func TestSpanPercentilesMergeAcrossShards(t *testing.T) {
	var rowsA, rowsB []*pb.LogRow
	for i := 1; i <= 1000; i++ {
		row := sdkSpanRow(i, "api", "op", fmt.Sprintf("s%d", i), "", "INFO", int64(i)*1_000_000, doneBody())
		if i%2 == 0 {
			rowsA = append(rowsA, row)
		} else {
			rowsB = append(rowsB, row)
		}
	}
	rows := theme.RenderSpanAggregate(runSpanQuery(t, "trace spans", newSpanShard(t, "pa", rowsA), newSpanShard(t, "pb", rowsB)))
	if len(rows) != 1 || rows[0].Count() != 1000 {
		t.Fatalf("rows = %+v", rows)
	}
	within := func(name string, got time.Duration, want time.Duration) {
		if diff := float64(got-want) / float64(want); diff > 0.011 || diff < -0.011 {
			t.Errorf("%s = %v, want %v within 1%%", name, got, want)
		}
	}
	within("p50", rows[0].P50, 500*time.Millisecond)
	within("p99", rows[0].P99, 990*time.Millisecond)
	if rows[0].Min != time.Millisecond || rows[0].Max != time.Second {
		t.Errorf("min/max = %v/%v, want exact 1ms/1s", rows[0].Min, rows[0].Max)
	}
}

func TestSpanRowsOfOneTraceKeepSelfTime(t *testing.T) {
	a := newSpanShard(t, "span-trace", []*pb.LogRow{
		sdkSpanRow(10, "api", "api.handle", "p", "", "INFO", 10_000_000, doneBody()),
		sdkSpanRow(8, "api", "db.query", "c", "p", "INFO", 4_000_000, doneBody()),
	})
	rows := theme.RenderSpanRollup(runSpanQuery(t, "trace t1 --spans", a))
	byName := map[string]theme.SpanRollupRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if got := byName["api.handle"].Occurrences[0].SelfDuration; got != 6*time.Millisecond {
		t.Errorf("api.handle self = %v, want 6ms (10ms minus its 4ms child)", got)
	}
}

func TestSpanSamplesAreTheSlowest(t *testing.T) {
	var rows []*pb.LogRow
	for i := 1; i <= 120; i++ {
		rows = append(rows, sdkSpanRow(i, "api", "op", fmt.Sprintf("s%d", i), "", "INFO", int64(i)*1_000_000, doneBody()))
	}
	got := theme.RenderSpanSamples(runSpanQuery(t, "trace spans op --samples", newSpanShard(t, "samples", rows)), theme.SpanSamplesLimit)
	if len(got) != theme.SpanSamplesLimit || got[0].Duration != 120*time.Millisecond || got[len(got)-1].Duration != 71*time.Millisecond {
		t.Fatalf("samples = %d, first %v, last %v; want the %d slowest", len(got), got[0].Duration, got[len(got)-1].Duration, theme.SpanSamplesLimit)
	}
}

func TestSpansPipeIntoJq(t *testing.T) {
	a := newSpanShard(t, "span-jq", []*pb.LogRow{
		sdkSpanRow(5, "api", "slow", "s1", "", "INFO", 3_000_000_000, doneBody()),
		sdkSpanRow(6, "api", "fast", "s2", "", "INFO", 1_000_000, doneBody()),
	})
	out := runSpanQuery(t, `trace spans | jq -r 'select(.p99_ns > 1000000000) | .name'`, a)
	if len(out) != 1 || out[0] != "slow" {
		t.Fatalf("jq over trace spans = %q, want just slow", out)
	}
}

func TestSpanSamplesLimit_PlannerMatchesTheme(t *testing.T) {
	if planner.SpanSamplesLimit != theme.SpanSamplesLimit {
		t.Errorf("planner asks shards for %d samples, theme shows %d", planner.SpanSamplesLimit, theme.SpanSamplesLimit)
	}
}
