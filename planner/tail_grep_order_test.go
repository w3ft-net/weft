package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shape/shapetest"
)

// Pipeline order matters when the post-LIMIT filter is *inclusive*
// (negation, regex with no anchor on a low-cardinality field, etc.):
// `tail -n N | grep -v ERROR | wc -l` asks "of the last N rows, count
// those NOT matching ERROR." If the planner merged that into a single
// `WHERE NOT regexp_matches(raw, 'ERROR') ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT N`, each shard would
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
			wantSQL:     `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 2000) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantCompute: []string{"grep -v ERROR", "wc -l"},
		},
		{
			name:     "head then awk inequality (not limit-safe)",
			pipeline: `cat vimgym.log | head -n 100 | awk 'LEVEL != "ERROR"'`,
			// The inequality isn't limit-safe, so the whole filter runs
			// in compute and needs LEVEL's real per-row value.
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, level FROM logs WHERE namespace = ? ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 100`,
			wantCompute: []string{`awk 'LEVEL != "ERROR"'`},
		},
		{
			name:        "tail then awk negated regex (not limit-safe)",
			pipeline:    `cat vimgym.log | tail -n 100 | awk '$0 !~ /ERROR/'`,
			wantSQL:     `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 100) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantCompute: []string{`awk '$0 !~ /ERROR/'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
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
	plan, err := Plan(pipeline, shapetest.Tenant(3), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs` + shapetest.Where() + ` ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 100) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`
	if plan.ShardSQL != want {
		t.Errorf("ShardSQL must place customer predicate before the LIMIT\n got: %s\nwant: %s", plan.ShardSQL, want)
	}
}

// TestPushdown_HardLimitBlocksPredicatePushdown pins the rule: once a
// head/tail line count has frozen the LIMIT, no later predicate is
// pushed into shard SQL, selective or not. `tail -n N | filter` means
// "the last N rows, then filter" — not "the last N rows that match" —
// so `tail -n 500 | awk 'LEVEL == "ERROR"' | wc -l` counts errors among
// 500 rows and can never exceed 500. The shard SQL carries only the
// LIMIT scan; every later stage runs in compute on the merged stream.
func TestPushdown_HardLimitBlocksPredicatePushdown(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantLimit   string // fragment the SQL must carry
		notInSQL    string // predicate that must NOT have been pushed
		wantCompute []string
	}{
		{
			name:        "tail then awk meta-var equality",
			pipeline:    `cat vimgym.log | tail -n 100 | awk 'LEVEL == "ERROR"'`,
			wantLimit:   "ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 100",
			notInSQL:    "level = 'ERROR'",
			wantCompute: []string{`awk 'LEVEL == "ERROR"'`},
		},
		{
			name:        "tail then awk positional equality",
			pipeline:    `cat vimgym.log | tail -n 100 | awk '$3 == "foo"'`,
			wantLimit:   "ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 100",
			notInSQL:    "list_extract",
			wantCompute: []string{`awk '$3 == "foo"'`},
		},
		{
			name:        "tail then plain grep",
			pipeline:    `cat vimgym.log | tail -n 2000 | grep ERROR | wc -l`,
			wantLimit:   "ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 2000",
			notInSQL:    "regexp_matches",
			wantCompute: []string{"grep ERROR", "wc -l"},
		},
		{
			name:        "head then awk regex",
			pipeline:    `cat vimgym.log | head -n 100 | awk '$0 ~ /^ERROR/'`,
			wantLimit:   "ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 100",
			notInSQL:    "regexp_matches",
			wantCompute: []string{`awk '$0 ~ /^ERROR/'`},
		},
		{
			name:        "head source then awk equality",
			pipeline:    `head -50 vimgym.log | awk 'LEVEL == "ERROR"'`,
			wantLimit:   "ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 50",
			notInSQL:    "level = 'ERROR'",
			wantCompute: []string{`awk 'LEVEL == "ERROR"'`},
		},
		{
			name:        "tail then sed selection",
			pipeline:    `cat vimgym.log | tail -n 100 | sed -n '/ERROR/p'`,
			wantLimit:   "ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 100",
			notInSQL:    "regexp_matches",
			wantCompute: []string{`sed -n '/ERROR/p'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantLimit) {
				t.Errorf("ShardSQL lost the limit scan %q:\n%s", tt.wantLimit, plan.ShardSQL)
			}
			if strings.Contains(plan.ShardSQL, tt.notInSQL) {
				t.Errorf("predicate %q was pushed past the hard limit:\n%s", tt.notInSQL, plan.ShardSQL)
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages: got %v, want %v", plan.ComputeStages, tt.wantCompute)
			}
			for i := range tt.wantCompute {
				if plan.ComputeStages[i] != tt.wantCompute[i] {
					t.Errorf("ComputeStages[%d]: got %q, want %q", i, plan.ComputeStages[i], tt.wantCompute[i])
				}
			}
		})
	}
}

// A filter BEFORE the line count still pushes: `grep ERROR | tail -n N`
// asks for the last N matching rows, which is exactly the SQL
// `WHERE ... ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT N`.
func TestPushdown_PreLimitFilterStillPushes(t *testing.T) {
	plan, err := planBare(`cat vimgym.log | grep ERROR | tail -n 100`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.ShardSQL, "regexp_matches(raw, 'ERROR')") || !strings.Contains(plan.ShardSQL, "LIMIT 100") {
		t.Errorf("pre-limit grep should push into the SQL ahead of the LIMIT:\n%s", plan.ShardSQL)
	}
}

// A dynamic regex (`~ "str"`, a variable) is not pushed into SQL: only a
// /regex/ literal is. It must survive as a compute stage verbatim for
// warp's awk to evaluate; dropping or mis-pushing it would silently match
// everything or nothing.
func TestPushdown_DynamicRegexStaysInCompute(t *testing.T) {
	for _, stage := range []string{
		`awk '$0 ~ "ERROR"'`,
		`awk 'SERVICE ~ "api"'`,
		`awk '$0 !~ "ERR" "OR"'`,
	} {
		plan, err := planBare(`cat vimgym.log | `+stage, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("Plan(%s): %v", stage, err)
		}
		if strings.Contains(plan.ShardSQL, "regexp_matches") {
			t.Errorf("%s: dynamic pattern was pushed into SQL:\n%s", stage, plan.ShardSQL)
		}
		if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != stage {
			t.Errorf("%s: ComputeStages = %v, want the stage verbatim", stage, plan.ComputeStages)
		}
	}
}
