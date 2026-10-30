package planner

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestClassifyShardMode_A_NoSort covers Mode A: no `sort` anywhere in
// the compute pipeline. ShardOrderBy is "ts" (with direction following
// a terminal head/tail), and ShardLimit mirrors Plan.Limit when
// head/tail pushed down, else 0.
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
			wantOrd:  "ts",
			wantLim:  0,
		},
		{
			name:     "cat + grep only",
			pipeline: `cat vimgym.log | grep ERROR`,
			wantOrd:  "ts",
			wantLim:  0,
		},
		{
			name:     "head 5 as source",
			pipeline: `head -5 vimgym.log`,
			wantOrd:  "ts",
			wantLim:  5,
		},
		{
			// tail's per-shard SQL still grabs the DESC top-N inside an
			// inner subquery, but the outer flip makes rows arrive at
			// compute in ts ASC. The merge key follows the wire-order,
			// not the inner pushdown.
			name:     "tail 10 as source",
			pipeline: `tail -10 vimgym.log`,
			wantOrd:  "ts",
			wantLim:  10,
		},
		{
			name:     "cat + head stage",
			pipeline: `cat vimgym.log | head -20`,
			wantOrd:  "ts",
			wantLim:  20,
		},
		{
			name:     "filter + head stage pushes both",
			pipeline: `cat vimgym.log | grep timeout | head -7`,
			wantOrd:  "ts",
			wantLim:  7,
		},
		{
			name:     "awk END block stays in compute — no sort → still Mode A",
			pipeline: `cat vimgym.log | awk '{ count++ } END { print count }'`,
			wantOrd:  "ts",
			wantLim:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
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
			name:     "awk filter pushes down, sort is first compute stage",
			pipeline: `cat vimgym.log | awk 'LEVEL == "ERROR"' | sort`,
			wantOrd:  "raw",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
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
			pipeline: `cat vimgym.log | awk '{ print toupper($1) }' | sort`,
		},
		{
			name:     "awk END block before sort",
			pipeline: `cat vimgym.log | awk '{ count++ } END { print count }' | sort`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
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

// TestDirectShardSQL verifies the SQL compute sends to shards
// uses ShardOrderBy/ShardLimit instead of
// the head/tail-derived ts ASC/DESC, and omits ORDER BY entirely in
// Mode C.
func TestDirectShardSQL(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name:     "mode A no head/tail — appends ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST",
			pipeline: `cat vimgym.log | grep ERROR`,
			want:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR') ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
		},
		{
			name:     "mode A with head — ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT N",
			pipeline: `cat vimgym.log | head -5`,
			want:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 5`,
		},
		{
			// tail wraps inner DESC+LIMIT in an outer ASC re-emit so the
			// merge key (and the user-facing output) is ts ASC, not DESC.
			// The outer SELECT is a cheap re-sort of the bounded inner.
			name:     "mode A with tail — subquery flips to ts ASC display",
			pipeline: `tail -10 vimgym.log`,
			want:     `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 10) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
		},
		{
			name:     "mode B plain sort — ORDER BY raw",
			pipeline: `cat vimgym.log | sort`,
			want:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY raw`,
		},
		{
			name:     "mode B sort -r — ORDER BY raw DESC",
			pipeline: `cat vimgym.log | sort -r`,
			want:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY raw DESC`,
		},
		{
			name:     "mode C sort -n — no ORDER BY",
			pipeline: `cat vimgym.log | sort -n`,
			want:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
		},
		{
			// The awk print folds into the SELECT projection so the
			// only compute stage is `sort` — first stage → Mode B.
			// The shard emits pre-projected raw values so ORDER BY
			// raw produces the same global order `awk | sort` would.
			name:     "mode B after projection fold — sort-as-first stage",
			pipeline: `cat vimgym.log | awk '{ print LEVEL }' | sort`,
			want:     `SELECT COALESCE(CAST(level AS VARCHAR), '') as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? ORDER BY raw`,
		},
		{
			// awk END block is a genuine compute-side transform that
			// can't fold into SELECT, so sort is no longer the first
			// compute stage → Mode C.
			name:     "mode C — awk END block before sort",
			pipeline: `cat vimgym.log | awk '{ count++ } END { print count }' | sort`,
			want:     projectedSQL(fieldReads{}, `namespace = ?`, false),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
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
	plan, err := Plan(`cat vimgym.log | sort | head -5`, 0, "", time.Time{}, time.Time{})
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

// TestTailThenSort_PushesNewestNByTs pins that a leading tail limit keeps
// its per-shard "newest N by ts" pushdown no matter what a later sort
// (or a jq that may or may not fold into the projection) does.
// `tail -N | … | sort` means "sort the newest N rows", so each shard
// must return its ts DESC top-N and compute must keep the global newest
// N by ts (TailMode / KeepLast) before sort runs.
func TestTailThenSort_PushesNewestNByTs(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		wantLim  int
	}{
		{
			name:     "tail | sort",
			pipeline: `tail -10 vimgym.log | sort`,
			wantLim:  10,
		},
		{
			name:     "tail | jq -r | sort",
			pipeline: `tail -n 200 vimgym.log | jq -r '.msg' | sort`,
			wantLim:  200,
		},
		{
			name:     "tail | jq | sort",
			pipeline: `tail -n 200 vimgym.log | jq '.msg' | sort`,
			wantLim:  200,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			wantInner := "ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT " + strconv.Itoa(tc.wantLim)
			if !strings.Contains(plan.DirectShardSQL, wantInner) {
				t.Errorf("DirectShardSQL missing %q\n  got: %s", wantInner, plan.DirectShardSQL)
			}
			if strings.Contains(plan.DirectShardSQL, "ORDER BY raw") {
				t.Errorf("DirectShardSQL orders by raw; tail's limit must select by ts\n  got: %s", plan.DirectShardSQL)
			}
			if plan.ShardOrderBy != "ts" {
				t.Errorf("ShardOrderBy (merge key) = %q, want %q", plan.ShardOrderBy, "ts")
			}
			if plan.ShardLimit != tc.wantLim {
				t.Errorf("ShardLimit = %d, want %d", plan.ShardLimit, tc.wantLim)
			}
			if !plan.TailMode {
				t.Errorf("TailMode = false; compute must keep the newest N by ts, not the last N to arrive")
			}
			if plan.Limit != tc.wantLim {
				t.Errorf("Limit = %d, want %d", plan.Limit, tc.wantLim)
			}
		})
	}
}
