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
