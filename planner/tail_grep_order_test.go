package planner

import (
	"testing"
	"time"
)

// Pipeline order matters when the post-LIMIT filter is *inclusive*
// (negation, regex with no anchor on a low-cardinality field, etc.):
// `tail -n N | grep -v ERROR | wc -l` asks "of the last N rows, count
// those NOT matching ERROR." If the planner merged that into a single
// `WHERE NOT regexp_matches(raw, 'ERROR') ORDER BY ts DESC LIMIT N`, each shard would
// return N non-ERROR rows, the cross-shard merge would trim back to N,
// and the count saturates at N regardless of how many rows actually
// have ERROR. See docs/plans/cross-shard-post-limit-filter.md.
//
// Selective filters — field equality, positive regex, plain grep
// PATTERN — DO push past the LIMIT now (see
// TestPushdown_LimitSafePushdown). The user's intent on
// `tail | awk '$N == "foo"'` is "recent matching rows," and shard-side
// filtering delivers that without a network roundtrip. The saturation
// hazard the doc warned about is specifically about *inclusive*
// predicates — those still go to compute.
//
// This test pins the hazard cases: any post-LIMIT filter that's NOT
// limit-safe must stay in compute and leave the SQL with just the
// LIMIT scan.
func TestPushdown_LimitBeforeFilter_DoesNotMerge(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		{
			name:        "tail then inverted grep (saturation hazard)",
			pipeline:    `cat vimgym.log | tail -n 2000 | grep -v ERROR | wc -l`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts DESC LIMIT 2000) AS t ORDER BY ts ASC`,
			wantCompute: []string{"grep -v ERROR", "wc -l"},
		},
		{
			name:     "head then awk inequality (not limit-safe)",
			pipeline: `cat vimgym.log | head -n 100 | awk 'LEVEL != "ERROR"'`,
			// The inequality isn't limit-safe, so the whole filter runs
			// in compute and needs LEVEL's real per-row value — see
			// docs/known-issues.md's (fixed) awk meta-variable entry.
			wantSQL:     `SELECT raw, ts, namespace, service, kind, level, instance, deployment, env, version, region, cluster, k8s_namespace, pod, container, node, trace_id, span_name, span_id, duration_ns FROM logs WHERE namespace = ? ORDER BY ts ASC LIMIT 100`,
			wantCompute: []string{`awk 'LEVEL != "ERROR"'`},
		},
		{
			name:        "tail then awk negated regex (not limit-safe)",
			pipeline:    `cat vimgym.log | tail -n 100 | awk '$0 !~ /ERROR/'`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts DESC LIMIT 100) AS t ORDER BY ts ASC`,
			wantCompute: []string{`awk '$0 !~ /ERROR/'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
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

// All-apps wildcard `cat warp.log | tail -n N | grep -v ERROR` emits
// a flat SQL with no source predicate; the LIMIT is pushed and the
// inverted grep runs in compute (saturation hazard — see above).
// Plan must still attach the customer predicate to bound the
// per-shard scan — otherwise a noisy neighbor's rows get pulled into
// LIMIT N alongside this customer's and silently shrink what they
// see (no leak, but skewed view). Uses an inclusive filter so the
// compute-side ordering invariant survives the test.
func TestPlanCustomer_BoundsPerShardLimit(t *testing.T) {
	pipeline := `cat warp.log | tail -n 100 | grep -v ERROR | wc -l`
	plan, err := Plan(pipeline, "victim-co", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE customer = ? ORDER BY ts DESC LIMIT 100) AS t ORDER BY ts ASC`
	if plan.ShardSQL != want {
		t.Errorf("ShardSQL must place customer predicate before the LIMIT\n got: %s\nwant: %s", plan.ShardSQL, want)
	}
}

// TestPushdown_LimitSafePushdown is the counterpart to
// TestPushdown_LimitBeforeFilter_DoesNotMerge: selective filters
// (field equality, positive regex, plain grep) MUST push past the
// LIMIT freeze. The user's intent on `tail | awk '$N == "foo"'` is
// "recent rows where field N matches"; making them go through compute
// would force every shard to ship its full LIMIT-N window across the
// wire only for compute to throw most of it away.
func TestPushdown_LimitSafePushdown(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		{
			name:        "tail then awk meta-var equality",
			pipeline:    `cat vimgym.log | tail -n 100 | awk 'LEVEL == "ERROR"'`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' ORDER BY ts DESC LIMIT 100) AS t ORDER BY ts ASC`,
			wantCompute: nil,
		},
		{
			name:        "tail then awk positional equality",
			pipeline:    `cat vimgym.log | tail -n 100 | awk '$3 == "foo"'`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? AND list_extract(fields, 3) = 'foo' ORDER BY ts DESC LIMIT 100) AS t ORDER BY ts ASC`,
			wantCompute: nil,
		},
		{
			name:        "tail then plain grep (positive)",
			pipeline:    `cat vimgym.log | tail -n 2000 | grep ERROR | wc -l`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR') ORDER BY ts DESC LIMIT 2000) AS t ORDER BY ts ASC`,
			wantCompute: []string{"wc -l"},
		},
		{
			name:        "head then awk regex",
			pipeline:    `cat vimgym.log | head -n 100 | awk '$0 ~ /^ERROR/'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, '^ERROR') ORDER BY ts ASC LIMIT 100`,
			wantCompute: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
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
