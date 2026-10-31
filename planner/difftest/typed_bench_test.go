package difftest

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// benchRows is a request log whose $2 is a latency in ms: log-normal, so most
// rows are small and a thin tail runs into the thousands. The shape matters
// because the base prefilter's string compare ships every row whose digits sort
// above the threshold, which for "$2 > 1000" is nearly every row.
const benchRows = 200_000

func latencyRows(n int) []*pb.LogRow {
	rng := rand.New(rand.NewSource(1))
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		ms := int(math.Exp(rng.NormFloat64()*1.1 + 4.6)) // median ~100ms
		rows[i] = &pb.LogRow{
			Raw:       fmt.Sprintf("req%07d %d GET /v1/items/%d status=200 bytes=%d", i, ms, rng.Intn(5000), rng.Intn(90000)),
			TsUnixNs:  base.Add(time.Duration(i) * time.Millisecond).UnixNano(),
			Namespace: "typed", Service: "api", Kind: "main",
		}
	}
	return rows
}

// BenchmarkTypedAwkFilter compares what a shard does for `awk '$2 > T'` with
// the base prefilter against the typed one: rows and bytes it hands back, and
// the time to produce them. Rows shipped is the cost that scales across a
// fleet (network, compute CPU for the awk stage that re-checks them).
//
//	go test -tags test -run xxx -bench TypedAwkFilter -benchtime 5x ./planner/difftest
func BenchmarkTypedAwkFilter(b *testing.B) {
	sh, err := shard.NewDuckDBShard("bench", "")
	if err != nil {
		b.Fatal(err)
	}
	defer sh.Close()
	rows := latencyRows(benchRows)
	for i := 0; i < len(rows); i += 20_000 {
		if err := sh.Ingest(context.Background(), rows[i:i+20_000], nil); err != nil {
			b.Fatal(err)
		}
	}

	for _, thr := range []string{"1000", "500", "100", "9"} {
		pipeline := fmt.Sprintf("cat /log/typed/api/main.log | awk '$2 > %s'", thr)
		plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
		if err != nil || plan.Typed == nil {
			b.Fatalf("%s: %v, typed %+v", pipeline, err, plan.Typed)
		}
		args := toAny(argStrings(plan.Args))
		for _, v := range []struct{ name, sql string }{{"base", plan.DirectShardSQL}, {"typed", plan.Typed.DirectShardSQL}} {
			b.Run(fmt.Sprintf("gt%s/%s", thr, v.name), func(b *testing.B) {
				var n, bytes int
				for i := 0; i < b.N; i++ {
					got, err := sh.Query(context.Background(), v.sql, args...)
					if err != nil {
						b.Fatal(err)
					}
					n, bytes = len(got), 0
					for _, r := range got {
						bytes += len(r.Raw)
					}
				}
				b.ReportMetric(float64(n), "rows_shipped")
				b.ReportMetric(float64(bytes)/1e6, "MB_shipped")
			})
		}
	}
}

func requestRows(n int) []*pb.LogRow {
	rng := rand.New(rand.NewSource(1))
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		ms := int(math.Exp(rng.NormFloat64()*1.1 + 4.6))
		rows[i] = &pb.LogRow{
			Raw:       fmt.Sprintf("req%07d GET %d /v1/items/%d status=200 bytes=%d", i, ms, rng.Intn(5000), rng.Intn(90000)),
			TsUnixNs:  base.Add(time.Duration(i) * time.Millisecond).UnixNano(),
			Namespace: "typed", Service: "api", Kind: "main",
		}
	}
	return rows
}

// BenchmarkAwkFilterPrint is `awk '$3 > T {print $1, $3}'`: the shard SQL for
// the base plan, the typed variant and the exact plan (no awk stage), against
// the same query handwritten over the stored fields column, and the whole query
// through compute with and without the awk stage.
//
//	go test -tags test -run xxx -bench AwkFilterPrint -benchtime 5x ./planner/difftest
func BenchmarkAwkFilterPrint(b *testing.B) {
	h := newHarness(b, requestRows(benchRows), 1)
	sh := h.shards[0]
	defer func() { h.unchecked = false }()
	for _, thr := range []string{"1000", "100"} {
		pipeline := fmt.Sprintf("cat /log/typed/api/main.log | awk '$3 > %s {print $1, $3}'", thr)
		plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
		if err != nil || plan.Typed == nil {
			b.Fatalf("%s: %v, typed %+v", pipeline, err, plan.Typed)
		}
		args := toAny(argStrings(plan.Args))
		// The record built from the fields column, with the exact filter: the
		// shard work a fully lowered typed plan would do.
		fieldsSQL := `SELECT list_extract(fields, 1) || ' ' || list_extract(fields, 3) AS raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs ` +
			`WHERE namespace = ? AND service = ? AND kind = ? ` +
			`AND TRY_CAST(list_extract(fields, 3) AS DOUBLE) > ` + thr + ` ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`

		shardQuery := func(sql string) func(*testing.B) {
			return func(b *testing.B) {
				var n int
				for i := 0; i < b.N; i++ {
					got, err := sh.Query(context.Background(), sql, args...)
					if err != nil {
						b.Fatal(err)
					}
					n = len(got)
				}
				b.ReportMetric(float64(n), "rows_shipped")
			}
		}
		b.Run("gt"+thr+"/shard/base", shardQuery(plan.DirectShardSQL))
		b.Run("gt"+thr+"/shard/typed", shardQuery(plan.Typed.DirectShardSQL))
		b.Run("gt"+thr+"/shard/exact", shardQuery(plan.Typed.Exact.DirectShardSQL))
		b.Run("gt"+thr+"/shard/fields", func(b *testing.B) {
			var n int
			for i := 0; i < b.N; i++ {
				got, err := sh.Query(context.Background(), fieldsSQL, args...)
				if err != nil {
					b.Fatal(err)
				}
				n = len(got)
			}
			b.ReportMetric(float64(n), "rows_shipped")
		})

		query := func(run func(*planner.QueryPlan) (string, error)) func(*testing.B) {
			return func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := run(plan); err != nil {
						b.Fatal(err)
					}
				}
			}
		}
		for _, run := range []func(*planner.QueryPlan) (string, error){h.runPlan, h.runTyped, h.runExact} {
			if _, err := run(plan); err != nil { // checked once, so a bad SQL is an error here
				b.Fatal(err)
			}
		}
		h.unchecked = true
		b.Run("gt"+thr+"/query/base", query(h.runPlan))
		b.Run("gt"+thr+"/query/typed", query(h.runTyped))
		b.Run("gt"+thr+"/query/exact", query(h.runExact))
	}
}

// BenchmarkFieldRowSpec is a stage that stays in compute and reads one field,
// `awk '{ s += $3 } END { print s }'`: a shard without the field form sending
// the whole lines (DirectShardSQL) against a shard returning the field's value
// (FieldSQL), and the whole query through compute either way.
//
//	go test -tags test -run xxx -bench FieldRowSpec -benchtime 5x ./planner/difftest
func BenchmarkFieldRowSpec(b *testing.B) {
	h := newHarness(b, requestRows(benchRows), 1)
	sh := h.shards[0]
	pipeline := "cat /log/typed/api/main.log | awk '{ s += $3 } END { print s }'"
	plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil || plan.FieldSQL == "" {
		b.Fatalf("%s: %v, field sql %q", pipeline, err, plan.FieldSQL)
	}
	args := toAny(argStrings(plan.Args))

	shardQuery := func(sql string) func(*testing.B) {
		return func(b *testing.B) {
			var n, bytes int
			for i := 0; i < b.N; i++ {
				got, err := sh.Query(context.Background(), sql, args...)
				if err != nil {
					b.Fatal(err)
				}
				n, bytes = len(got), 0
				for _, r := range got {
					bytes += len(r.Raw)
					for _, f := range r.Fields {
						bytes += len(f)
					}
				}
			}
			b.ReportMetric(float64(n), "rows_shipped")
			b.ReportMetric(float64(bytes)/1e6, "MB_values")
		}
	}
	for _, run := range []func(*planner.QueryPlan) (string, error){h.runPlan, h.runFieldForm} {
		if _, err := run(plan); err != nil { // checked once, so a bad SQL is an error here
			b.Fatal(err)
		}
	}
	h.unchecked = true
	b.Run("shard/whole-lines", shardQuery(plan.DirectShardSQL))
	b.Run("shard/field-form", shardQuery(plan.FieldSQL))
	b.Run("query/whole-lines", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := h.runPlan(plan); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("query/field-form", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := h.runFieldForm(plan); err != nil {
				b.Fatal(err)
			}
		}
	})
}
