package planner

import "testing"

// Pipeline order matters: `tail -n N | grep PAT` is "of the last N rows,
// keep those matching PAT" — the LIMIT bounds the input set, then the
// filter selects from inside it. If the planner merges them into a
// single SQL `WHERE raw LIKE 'PAT' ORDER BY ts DESC LIMIT N`, it
// silently rewrites that as "of all rows matching PAT, take the last
// N" — a strictly larger candidate set, so the count saturates at N
// whenever there are at least N matches in history.
//
// Caught in the multi-customer demo: `cat /log/acme/api.log | tail -n
// 2000 | grep ERROR | wc -l` returned 2000, and the inverted form
// `grep -v ERROR | wc -l` also returned 2000. The seeded `error-rate`
// watch (`tail -n 2000 ... | grep ERROR | wc -l | alert above 200`)
// is exactly this pattern; its dashboard value is meaningless until
// fixed.
//
// Option A (see docs/plans/cross-shard-post-limit-filter.md): the
// LIMIT pushes per-shard, the filter runs in compute on the global
// merged top-N stream. The earlier nested-SQL approach was correct
// per-shard but inflated cross-shard — each shard ran its own filter
// against its own LIMIT-N window, so the merge contained N×num_shards
// matches and the global cap (also N) trimmed back to N regardless
// of selectivity. Running the filter post-merge dodges that.
//
// Pre-limit filters (filter | tail) are unaffected — the freeze is
// triggered by the LIMIT pushdown itself, not by entering the loop.
// Most real watches look like `cat | filter | aggregate` and keep
// full pushdown; the loss is bounded to the narrow `head/tail | filter`
// shape, where compute receives ≤ N rows per shard (a few MB worst
// case, bounded by the LIMIT itself).
func TestPushdown_LimitBeforeFilter_DoesNotMerge(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		{
			name:        "tail then grep",
			pipeline:    `cat vimgym.log | tail -n 2000 | grep ERROR | wc -l`,
			wantSQL:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY ts DESC LIMIT 2000`,
			wantCompute: []string{"grep ERROR", "wc -l"},
		},
		{
			name:        "tail then inverted grep",
			pipeline:    `cat vimgym.log | tail -n 2000 | grep -v ERROR | wc -l`,
			wantSQL:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY ts DESC LIMIT 2000`,
			wantCompute: []string{"grep -v ERROR", "wc -l"},
		},
		{
			name:        "head then awk filter",
			pipeline:    `cat vimgym.log | head -n 100 | awk '$LEVEL == "ERROR"'`,
			wantSQL:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY ts ASC LIMIT 100`,
			wantCompute: []string{`awk '$LEVEL == "ERROR"'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages: got %d stages %v, want %d %v",
					len(plan.ComputeStages), plan.ComputeStages,
					len(tt.wantCompute), tt.wantCompute)
			}
			for i := range tt.wantCompute {
				if plan.ComputeStages[i] != tt.wantCompute[i] {
					t.Errorf("ComputeStages[%d]: got %q, want %q",
						i, plan.ComputeStages[i], tt.wantCompute[i])
				}
			}
		})
	}
}

// All-apps wildcard `cat warp.log | tail -n N | grep ERROR` emits a
// flat SQL with no source predicate; the LIMIT is pushed and the
// filter runs in compute. InjectCustomer must still attach a
// customer predicate to bound the per-shard scan — otherwise a noisy
// neighbor's rows get pulled into LIMIT N alongside this customer's
// and silently shrink what they see (no leak, but skewed view).
func TestInjectCustomer_BoundsPerShardLimit(t *testing.T) {
	pipeline := `cat warp.log | tail -n 100 | grep ERROR | wc -l`
	plan, err := Plan(pipeline)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := `SELECT line_num, raw FROM logs ORDER BY ts DESC LIMIT 100`
	if plan.ShardSQL != want {
		t.Fatalf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, want)
	}
	injected := InjectCustomer(plan.ShardSQL, "victim-co")
	wantInjected := `SELECT line_num, raw FROM logs WHERE customer = 'victim-co' ORDER BY ts DESC LIMIT 100`
	if injected != wantInjected {
		t.Errorf("InjectCustomer must bound the LIMIT scan\n got: %s\nwant: %s", injected, wantInjected)
	}
}
