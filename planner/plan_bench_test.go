package planner

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"
)

// benchFixtures are the representative watch shapes we measure Plan()
// over. The intent is to span the actual cost surface a real prod
// watch hits — not every edge case, but at least one of each shape
// (bare source, single-filter, subquery-flip from tail, multi-stage
// with limit-safe pushdown, trailing alert, jq projection). The
// fixture cardinality matters less than coverage of the pushdown
// paths in Plan().
var benchFixtures = []struct {
	name     string
	pipeline string
}{
	{"bare_cat", `cat /log/loom/api/prod.log`},
	{"cat_grep", `cat /log/loom/api/prod.log | grep ERROR`},
	{"tail_subquery", `tail -n 100 /log/loom/api/prod.log`},
	{"multi_stage_limit_safe", `cat /log/loom/api/prod.log | grep ERROR | awk 'LEVEL == "ERROR"' | wc -l`},
	{"alert_pipeline", `cat /log/loom/api/prod.log | grep ERROR | alert above 5`},
	{"jq_select", `cat /log/loom/api/prod.log | jq 'select(.level == "error")'`},
}

// benchTimeBounds is the time window we pass to Plan. Fixed so the
// constant overhead of constructing time.Time values doesn't leak
// into the per-op number, and so multiple runs are comparable.
var (
	benchStart = time.Date(2026, 5, 13, 9, 0, 0, 0, time.UTC)
	benchEnd   = time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
)

// BenchmarkPlan measures the cost of one planner.Plan() call per
// representative pipeline. This is the work that today repeats on
// every cron fire (and every interactive query). If the per-op
// number is small enough, save-time lowering isn't worth the schema
// churn. The decision gate is in the task plan.
//
// Run with:
//
//	go test -bench=BenchmarkPlan -benchmem -benchtime=10x ./planner/
func BenchmarkPlan(b *testing.B) {
	for _, fx := range benchFixtures {
		b.Run(fx.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				plan, err := Plan(fx.pipeline, shapetest.Tenant(1), "", benchStart, benchEnd)
				if err != nil {
					b.Fatalf("Plan(%q): %v", fx.pipeline, err)
				}
				// Touch a field so the compiler doesn't elide the call.
				_ = plan.ShardSQL
			}
		})
	}
}
