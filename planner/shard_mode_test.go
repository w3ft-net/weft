package planner

import "testing"

// TestClassifyShardMode_A_NoSort covers Mode A: no `sort` anywhere in
// the compute pipeline. ShardOrderBy is "line_num" (with direction
// following a terminal head/tail), and ShardLimit mirrors Plan.Limit
// when head/tail pushed down, else 0.
func TestClassifyShardMode_A_NoSort(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		wantOrd  string
		wantLim  int
	}{
		{
			name:     "plain cat (no head/tail, no sort)",
			pipeline: `cat vimgym.log`,
			wantOrd:  "line_num",
			wantLim:  0,
		},
		{
			name:     "cat + grep only",
			pipeline: `cat vimgym.log | grep ERROR`,
			wantOrd:  "line_num",
			wantLim:  0,
		},
		{
			name:     "head 5 as source",
			pipeline: `head -5 vimgym.log`,
			wantOrd:  "line_num",
			wantLim:  5,
		},
		{
			name:     "tail 10 as source",
			pipeline: `tail -10 vimgym.log`,
			wantOrd:  "line_num DESC",
			wantLim:  10,
		},
		{
			name:     "cat + head stage",
			pipeline: `cat vimgym.log | head -20`,
			wantOrd:  "line_num",
			wantLim:  20,
		},
		{
			name:     "filter + head stage pushes both",
			pipeline: `cat vimgym.log | grep timeout | head -7`,
			wantOrd:  "line_num",
			wantLim:  7,
		},
		{
			name:     "awk END block stays in compute — no sort → still Mode A",
			pipeline: `cat vimgym.log | awk '{ count++ } END { print count }'`,
			wantOrd:  "line_num",
			wantLim:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.ShardOrderBy != tc.wantOrd {
				t.Errorf("ShardOrderBy = %q, want %q", plan.ShardOrderBy, tc.wantOrd)
			}
			if plan.ShardLimit != tc.wantLim {
				t.Errorf("ShardLimit = %d, want %d", plan.ShardLimit, tc.wantLim)
			}
		})
	}
}

// TestClassifyShardMode_B_PlainSortIsFirstCompute covers Mode B: the
// compute pipeline begins with a plain `sort` / `sort -r` (no flags
// other than -r). ShardOrderBy is "raw" / "raw DESC".
func TestClassifyShardMode_B_PlainSortIsFirstCompute(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		wantOrd  string
	}{
		{
			name:     "sort as only compute stage",
			pipeline: `cat vimgym.log | sort`,
			wantOrd:  "raw",
		},
		{
			name:     "sort -r as only compute stage",
			pipeline: `cat vimgym.log | sort -r`,
			wantOrd:  "raw DESC",
		},
		{
			name:     "filter pushes down, sort is first compute stage",
			pipeline: `cat vimgym.log | grep ERROR | sort`,
			wantOrd:  "raw",
		},
		{
			name:     "jq select pushes down, sort is first compute stage",
			pipeline: `cat vimgym.log | jq 'select(.status == 500)' | sort`,
			wantOrd:  "raw",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.ShardOrderBy != tc.wantOrd {
				t.Errorf("ShardOrderBy = %q, want %q", plan.ShardOrderBy, tc.wantOrd)
			}
			// Mode B with no terminal head/tail → ShardLimit=0.
			if plan.ShardLimit != 0 {
				t.Errorf("ShardLimit = %d, want 0", plan.ShardLimit)
			}
		})
	}
}

// TestClassifyShardMode_C covers cases that fall to Mode C: the shard
// does no ordering and no limit; compute handles everything.
func TestClassifyShardMode_C(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
	}{
		{
			name:     "sort -n (numeric)",
			pipeline: `cat vimgym.log | sort -n`,
		},
		{
			name:     "sort -k 3 (key column)",
			pipeline: `cat vimgym.log | sort -k 3`,
		},
		{
			name:     "sort -f (case-fold)",
			pipeline: `cat vimgym.log | sort -f`,
		},
		{
			name:     "transform before sort — awk print + sort",
			pipeline: `cat vimgym.log | awk '{ print $3, $INSTANCE }' | sort`,
		},
		{
			name:     "awk END block before sort",
			pipeline: `cat vimgym.log | awk '{ count++ } END { print count }' | sort`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.ShardOrderBy != "" {
				t.Errorf("ShardOrderBy = %q, want \"\" (Mode C)", plan.ShardOrderBy)
			}
			if plan.ShardLimit != 0 {
				t.Errorf("ShardLimit = %d, want 0 (Mode C)", plan.ShardLimit)
			}
		})
	}
}

// TestDirectShardSQL verifies op 6: the SQL the coordinator sends to
// shards via QueryToCompute uses ShardOrderBy/ShardLimit instead of
// the head/tail-derived ts ASC/DESC, and omits ORDER BY entirely in
// Mode C.
func TestDirectShardSQL(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name:     "mode A no head/tail — appends ORDER BY line_num",
			pipeline: `cat vimgym.log | grep ERROR`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND raw LIKE '%ERROR%' ORDER BY line_num`,
		},
		{
			name:     "mode A with head — ORDER BY line_num LIMIT N",
			pipeline: `cat vimgym.log | head -5`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY line_num LIMIT 5`,
		},
		{
			name:     "mode A with tail — ORDER BY line_num DESC LIMIT N",
			pipeline: `tail -10 vimgym.log`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY line_num DESC LIMIT 10`,
		},
		{
			name:     "mode B plain sort — ORDER BY raw",
			pipeline: `cat vimgym.log | sort`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY raw`,
		},
		{
			name:     "mode B sort -r — ORDER BY raw DESC",
			pipeline: `cat vimgym.log | sort -r`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY raw DESC`,
		},
		{
			name:     "mode C sort -n — no ORDER BY",
			pipeline: `cat vimgym.log | sort -n`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
		},
		{
			// The awk print folds into the SELECT projection so the
			// only compute stage is `sort` — first stage → Mode B.
			// The shard emits pre-projected raw values so ORDER BY
			// raw produces the same global order `awk | sort` would.
			name:     "mode B after projection fold — sort-as-first stage",
			pipeline: `cat vimgym.log | awk '{ print $3 }' | sort`,
			want:     `SELECT line_num, list_extract(fields, 3) as raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') ORDER BY raw`,
		},
		{
			// awk END block is a genuine compute-side transform that
			// can't fold into SELECT, so sort is no longer the first
			// compute stage → Mode C.
			name:     "mode C — awk END block before sort",
			pipeline: `cat vimgym.log | awk '{ count++ } END { print count }' | sort`,
			want:     `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.DirectShardSQL != tc.want {
				t.Errorf("DirectShardSQL\n  got: %s\n want: %s", plan.DirectShardSQL, tc.want)
			}
		})
	}
}

// TestClassifyShardMode_HeadAfterSort_StaysSafe — `sort | head -N` is
// Mode C in the MVP (we don't push LIMIT because that'd require the
// merge-direction XOR with sort's -r flag). The shard orders +
// limits nothing; compute sorts and then trims.
func TestClassifyShardMode_HeadAfterSort_StaysSafe(t *testing.T) {
	plan, err := Plan(`cat vimgym.log | sort | head -5`)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.ShardOrderBy != "raw" {
		t.Errorf("ShardOrderBy = %q, want \"raw\" (sort is first compute stage)", plan.ShardOrderBy)
	}
	// We conservatively keep ShardLimit=0 here: pushing the 5 is
	// safe in Mode B, but the plumbing for "head stage after a
	// sort" to mirror its limit onto shards lives in a follow-up.
	if plan.ShardLimit != 0 {
		t.Errorf("ShardLimit = %d, want 0 (no limit pushed when head follows sort)", plan.ShardLimit)
	}
}
