package coordinator

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// noopPool returns an empty stream immediately. Used by the cron-fire
// benchmark so we measure ONLY the coordinator-side work that save-
// time lowering would replace (preflight, expandBinPipeline, Plan,
// dispatch struct construction). The actual shard query time is what
// save-time lowering *cannot* eliminate — it lives downstream of the
// dispatch.
type noopPool struct{}

func (noopPool) ExecuteQuery(_ *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	return compute.NewSliceLineStream(nil, nil), nil
}

func (noopPool) NewJob(_ []string, _ int, _ string, _ int, _ bool) compute.ComputeJob {
	return nil
}

// BenchmarkExecuteForWatch measures the per-fire cost of the
// coordinator-side work: preflight (customer resolve + /bin expansion +
// glob expansion) and Plan(). The compute pool is a true no-op so the
// per-op number excludes any shard execution. This is the "lowering can
// hope to replace this much" upper bound.
//
// Two sub-benches:
//   - bare        : pipeline with no /bin reference (just preflight + Plan)
//   - with_bin    : pipeline that calls /bin/loom-error-rate (also exercises
//                   expandBinPipeline's bash parse + splice)
//
// Run with:
//
//	go test -bench=BenchmarkExecuteForWatch -benchmem -benchtime=1s ./coordinator/
func BenchmarkExecuteForWatch(b *testing.B) {
	c := setupTestCoordinator(b)
	c.Pool = noopPool{}

	// Resolve the customer ID the test fixture seeded.
	cust, err := c.Store.GetCustomerByName("kevlar")
	if err != nil {
		b.Fatalf("resolve test customer: %v", err)
	}

	// Save a /bin script the with_bin sub-bench references. The body
	// is a 2-pipeline shape that's typical of real /bin/* scripts —
	// not contrived to avoid pushdown.
	binScript := `cat /log/vimgym.log | grep ERROR | wc -l`
	if err := c.Store.SaveShellFunction(cust.ID, "", "loom-error-rate", binScript, "bash"); err != nil {
		b.Fatalf("seed /bin/loom-error-rate: %v", err)
	}

	cases := []struct {
		name     string
		pipeline string
		// lowered: when true, the bench seeds watch.LoweredPlan so the
		// fire path takes the save-time-lowered shortcut (no Plan,
		// no preflight, no GetCustomerByID DB hit). When false, the
		// fire path takes the inline-Plan fallback. Comparing the two
		// per-op numbers IS the Phase B win.
		lowered bool
	}{
		{name: "bare_inline", pipeline: `cat /log/vimgym.log | grep ERROR`, lowered: false},
		{name: "bare_lowered", pipeline: `cat /log/vimgym.log | grep ERROR`, lowered: true},
		{name: "with_bin_inline", pipeline: `/bin/loom-error-rate | head -5`, lowered: false},
		{name: "with_bin_lowered", pipeline: `/bin/loom-error-rate | head -5`, lowered: true},
	}

	tr := TimeRange{
		Start: t0, // shard fixture windows are [t0, t1] and [t1, t2]
		End:   t2,
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			w := &store.CronWatch{
				CustomerID: cust.ID,
				Name:       "bench-" + tc.name,
				Schedule:   "* * * * *",
				Pipeline:   tc.pipeline,
				Enabled:    true,
			}

			if tc.lowered {
				lp, err := planner.Lower(tc.pipeline, "kevlar", "bolt", noBinResolver{})
				if err != nil {
					// Pipelines that don't lower (e.g. /bin/ refs the
					// stub can't inline) skip the lowered sub-bench
					// rather than fail the entire run. With_bin will
					// land here today; remove this skip once Lower
					// gets full /bin inlining.
					b.Skipf("Lower not yet supported for %q: %v", tc.pipeline, err)
				}
				js, err := json.Marshal(lp)
				if err != nil {
					b.Fatalf("marshal lowered plan: %v", err)
				}
				w.LoweredPlan = js
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				res, err := c.ExecuteForWatch(tc.pipeline, tr, w)
				if err != nil {
					b.Fatalf("ExecuteForWatch: %v", err)
				}
				if res != nil && res.Stream != nil {
					_, _ = res.Drain(context.Background())
				}
			}
		})
	}
}
