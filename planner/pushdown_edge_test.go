package planner

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// ===========================================================================
// Edge cases and failure modes for predicate pushdown.
//
// Organized by subsystem, then by "should succeed" vs "should fail gracefully"
// vs "should refuse to push down".
// ===========================================================================

// ---------------------------------------------------------------------------
// Source parsing edge cases
// ---------------------------------------------------------------------------

func TestEdge_Source(t *testing.T) {
	t.Run("error cases", func(t *testing.T) {
		errorCases := []struct {
			name     string
			pipeline string
		}{
			{"empty string", ``},
			{"cat with no file", `cat`},
			{"head with no file", `head`},
			{"tail with no file", `tail`},
		}
		for _, tt := range errorCases {
			t.Run(tt.name, func(t *testing.T) {
				_, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
				if err == nil {
					t.Errorf("Plan(%q): expected error, got nil", tt.pipeline)
				}
			})
		}
	})

	t.Run("local pipeline", func(t *testing.T) {
		_, err := planBare("echo 42", "", time.Time{}, time.Time{})
		if !errors.Is(err, ErrNotPlannable) {
			t.Fatalf("Plan(\"echo 42\") err = %v, want ErrNotPlannable", err)
		}
	})

	// A leading command that isn't cat/head/tail but is still handed a
	// /log/ path (real-Unix habit: `grep pattern file` instead of piping
	// through a source stage first) must fail loudly, not get silently
	// reclassified as ErrNotPlannable and routed to plain warp execution
	// against shellfs's LogMount, where it returns zero rows with no
	// error at all.
	t.Run("non-source leading command against a /log/ path", func(t *testing.T) {
		_, err := planBare(`grep ERROR /log/weft/coordinator/main.log`, "", time.Time{}, time.Time{})
		if err == nil {
			t.Fatal("Plan(grep ERROR /log/...): expected error, got nil")
		}
		if errors.Is(err, ErrNotPlannable) {
			t.Fatalf("Plan(grep ERROR /log/...) err = %v, want a clear rejection, not ErrNotPlannable (that silently routes to warp)", err)
		}
		if !strings.Contains(err.Error(), "cat") || !strings.Contains(err.Error(), "head") || !strings.Contains(err.Error(), "tail") {
			t.Fatalf("Plan(grep ERROR /log/...) err = %v, want it to name cat/head/tail as the fix", err)
		}
	})

	t.Run("different app names", func(t *testing.T) {
		tests := []struct {
			name     string
			pipeline string
			wantSQL  string
		}{
			{
				name:     "callbox app",
				pipeline: `cat callbox.log`,
				wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			},
			{
				name:     "hyphenated app name",
				pipeline: `cat my-service.log`,
				wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			},
			{
				name:     "underscored app name",
				pipeline: `cat my_service.log`,
				wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			},
			{
				name:     "head on warp.log",
				pipeline: `head warp.log`,
				wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 10`,
			},
			{
				name:     "tail on warp.log",
				pipeline: `tail -50 warp.log`,
				wantSQL:  `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 50) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
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
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Awk edge cases
// ---------------------------------------------------------------------------

func TestEdge_Awk_NonPushable(t *testing.T) {
	// These awk programs should NOT push down at all — the entire stage
	// becomes compute.
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// BEGIN block: no pushable filter
		{
			name:        "BEGIN block only",
			pipeline:    `cat vimgym.log | awk 'BEGIN { print "header" }'`,
			wantSQL:     plainSQL(`namespace = ?`),
			wantCompute: []string{`awk 'BEGIN { print "header" }'`},
		},
		// No pattern at all, just an action on every line
		{
			name:        "unconditional print action",
			pipeline:    `cat vimgym.log | awk '{ print $2 }'`,
			wantSQL:     `SELECT COALESCE(list_extract(fields, 2), '') as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: nil,
		},
		// User-defined function
		{
			name:        "program with function definition",
			pipeline:    `cat vimgym.log | awk 'function foo() { return 1 } /test/'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk 'function foo() { return 1 } /test/'`},
		},
		// Assignment in pattern position — not a filter
		{
			name:        "assignment expression is not a filter",
			pipeline:    `cat vimgym.log | awk '{ x = $3 + 1; print x }'`,
			wantSQL:     plainSQL(`namespace = ?`),
			wantCompute: []string{`awk '{ x = $3 + 1; print x }'`},
		},
		// Non-pushable field access: a variable index other than NF.
		{
			name:        "variable field index",
			pipeline:    `cat vimgym.log | awk '$i == "ERROR"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk '$i == "ERROR"'`},
		},
		// Field compared to another field — can't push down
		{
			name:        "field vs field comparison",
			pipeline:    `cat vimgym.log | awk '$3 > $4'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk '$3 > $4'`},
		},
		// Function call in pattern — can't push down (length is the one that does)
		{
			name:        "function call in pattern",
			pipeline:    `cat vimgym.log | awk 'toupper($1) == "ERROR"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk 'toupper($1) == "ERROR"'`},
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// TestEdge_Awk_FilterAfterLimitFreezeMustSurvive documents the
// post-`tail` / post-`head` awk filter case that user-facing queries
// actually hit:
//
//	tail /log/weft/coordinator/main.log | awk '$4 == "compute" {print}'
//
// Two facts collide here. (1) After tail freezes the LIMIT, every
// subsequent stage MUST run in compute — pushing a filter shard-side
// post-LIMIT lets cross-shard merge re-saturate the result. (2) extractRawExpr's
// awkProgramToSQL helper looks at the print action only; for a bare
// `{print}` it returns ("raw", true) — meaning "this stage is
// equivalent to projecting raw, drop it." But the FILTER pattern
// `$4 == "compute"` was attached to that action; declaring the stage
// redundant silently strips the filter.
//
// Result: the planner returns ShardSQL with no awk predicate AND
// empty ComputeStages. cmd/explain even cheerfully reports "Compute
// stages: (none — fully pushed down)." The shard returns the last 10
// rows ordered by ts DESC, none of them filtered.
//
// Invariant for any pipeline containing an awk pattern: the filter
// pattern's predicate has to appear *somewhere* — as a shard SQL
// predicate (when pushdown is safe), as a compute stage (when not),
// or as a projection that incorporates the filter. It must NOT
// silently disappear.
func TestEdge_Awk_FilterAfterLimitFreezeMustSurvive(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
	}{
		{
			name:     "tail then awk positional equality with explicit print",
			pipeline: `tail /log/weft/coordinator/main.log | awk '$4 == "compute" {print}'`,
		},
		{
			name:     "head then awk positional equality with explicit print",
			pipeline: `head /log/weft/coordinator/main.log | awk '$3 == "compute" {print}'`,
		},
		{
			name:     "tail then awk positional equality with default action",
			pipeline: `tail /log/weft/coordinator/main.log | awk '$4 == "compute"'`,
		},
		{
			name:     "tail then awk regex on $0 with explicit print",
			pipeline: `tail /log/weft/coordinator/main.log | awk '$0 ~ /compute/ {print}'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}

			// The filter must survive as either a SQL predicate (the
			// `list_extract(fields, N) = '…'`, `level = '…'`, or
			// `regexp_matches(raw, '…')` shape) or as a compute stage carrying the
			// original awk program. Anywhere is fine — but somewhere
			// is required.
			pushedToSQL := strings.Contains(plan.ShardSQL, "list_extract(fields,") ||
				strings.Contains(plan.ShardSQL, "regexp_matches(raw,") ||
				strings.Contains(plan.ShardSQL, "level =") ||
				strings.Contains(plan.ShardSQL, "raw LIKE")
			pushedToCompute := false
			for _, s := range plan.ComputeStages {
				if strings.Contains(s, "awk") {
					pushedToCompute = true
					break
				}
			}
			if !pushedToSQL && !pushedToCompute {
				t.Errorf("filter silently dropped\n  ShardSQL = %s\n  ComputeStages = %v",
					plan.ShardSQL, plan.ComputeStages)
			}
		})
	}
}

func TestEdge_Awk_Pushable(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// $0 references (whole record)
		{
			name:     "field zero equals string",
			pipeline: `cat vimgym.log | awk '$0 == "exact match"'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND raw = 'exact match'`,
		},
		// $NF is the last field, or $0 on a line with none.
		{
			name:     "last field equals string",
			pipeline: `cat vimgym.log | awk '$NF == "ERROR"'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(list_extract(fields, -1), raw) = 'ERROR'`,
		},
		// length counts bytes, as the awk that runs the stage does.
		{
			name:     "length of the record",
			pipeline: `cat vimgym.log | awk 'length($0) > 100'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND strlen(raw) > 100`,
		},
		{
			name:     "bare length of a field",
			pipeline: `cat vimgym.log | awk 'length($3) == 0'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(strlen(COALESCE(list_extract(fields, 3), '')), 0) = 0`,
		},
		// Meta-var less-than (non-standard but expressible)
		{
			name:     "meta-var less than string",
			pipeline: `cat vimgym.log | awk 'LEVEL < "WARN"'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(level, '') < 'WARN'`,
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
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Awk partial pushdown edge cases
// ---------------------------------------------------------------------------

func TestEdge_Awk_PartialPushdown(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// Pattern + printf (not print)
		{
			name:        "filter with printf action",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR" { printf "%s %s\n", $1, $2 }'`,
			wantSQL:     plainSQL(`namespace = ? AND level = 'ERROR'`, "level"),
			wantCompute: []string{`awk 'LEVEL == "ERROR" { printf "%s %s\n", $1, $2 }'`},
		},
		// Regex pattern + non-default action
		{
			name:        "regex pattern with print action",
			pipeline:    `cat vimgym.log | awk '/timeout/ { print $1, $2 }'`,
			wantSQL:     `SELECT concat_ws(' ', COALESCE(list_extract(fields, 1), ''), COALESCE(list_extract(fields, 2), '')) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
			wantCompute: nil,
		},
		// Filter + increment + END (count errors)
		{
			name:        "count pattern with END",
			pipeline:    `cat vimgym.log | awk '/error/ { c++ } END { print c }'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'error')`,
			wantCompute: []string{`awk '/error/ { c++ } END { print c }'`},
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Jq edge cases
// ---------------------------------------------------------------------------

// TestEdge_Jq_MetaVarOutputRead pins the fix for the known-issues.md
// entry "jq's reserved meta-variables ... resolve to null when read as
// output": a bare "$host"-shaped read (not inside a pushable select())
// falls through to the jq compute-stage evaluator, which only sees the
// row's raw JSON body — so the planner must project the backing
// columns it reads alongside raw whenever such a read is present, for
// compute (compute/metavars.go) to splice into each row before the jq
// stage runs. Only those: a row has about twenty and a stage reads one or two.
func TestEdge_Jq_MetaVarOutputRead(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantCols    []string
		wantCompute []string
	}{
		{
			name:        "host read with -r, piped on",
			pipeline:    `cat vimgym.log | jq -r '$host | ascii_downcase'`,
			wantCols:    []string{"instance"},
			wantCompute: []string{`jq -r '$host | ascii_downcase'`},
		},
		{
			name:        "host read inside string interpolation",
			pipeline:    `cat vimgym.log | jq -r 'select($level == "INFO") | "\($host) \(.pct)"'`,
			wantCols:    []string{"level", "instance"},
			wantCompute: []string{`jq -r 'select($level == "INFO") | "\($host) \(.pct)"'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			for _, col := range MetaVarColumns {
				if got, want := hasColumn(plan.ShardSQL, col), slices.Contains(tt.wantCols, col); got != want {
					t.Errorf("ShardSQL has meta-var column %q = %v, want %v: %s", col, got, want, plan.ShardSQL)
				}
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// TestEdge_Jq_NoMetaVarColumnsWithoutMetaVarRead guards the other
// direction: a jq stage that reads no meta-variable at all must NOT
// pay for the extra columns — TestEdge_Jq_NonPushable already pins the
// exact SQL for several such cases; this just states the invariant
// explicitly for a stage sharing the "jq" prefix that could tempt a
// cruder (name-only) detection heuristic into a false positive.
func TestEdge_Jq_NoMetaVarColumnsWithoutMetaVarRead(t *testing.T) {
	plan, err := planBare(`cat vimgym.log | jq -r '.pct'`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, col := range MetaVarColumns {
		if strings.Contains(plan.ShardSQL, ", "+col) {
			t.Errorf("ShardSQL unexpectedly carries meta-var column %q: %s", col, plan.ShardSQL)
		}
	}
}

// TestEdge_Awk_MetaVarInUnpushedStage pins that a bare meta-variable name
// (not $-prefixed — see planner/awk.go's metaVars doc comment for why)
// referenced anywhere in a compute stage that doesn't fully push down
// — filter or output — gets the real per-row value once it reaches
// warp's awk evaluator, which otherwise has no idea what "HOST" means.
func TestEdge_Awk_MetaVarInUnpushedStage(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantCols    []string
		wantCompute []string
	}{
		{
			// An `if` inside the action defeats the single-print-
			// statement raw-expr fold (extractAwkRawExpr), so the
			// whole stage stays in compute and needs HOST's real value
			// for both the if-condition and the print.
			name:        "output read via print inside an if",
			pipeline:    `cat vimgym.log | awk '{ if (HOST == "web-1") print $1 }'`,
			wantCols:    []string{"instance"},
			wantCompute: []string{`awk '{ if (HOST == "web-1") print $1 }'`},
		},
		{
			name:        "filter re-evaluated after NR mixing blocks pushdown",
			pipeline:    `cat vimgym.log | awk 'NR >= 10 && HOST == "web-1"'`,
			wantCols:    []string{"instance"},
			wantCompute: []string{`awk 'NR >= 10 && HOST == "web-1"'`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			for _, col := range MetaVarColumns {
				if got, want := hasColumn(plan.ShardSQL, col), slices.Contains(tt.wantCols, col); got != want {
					t.Errorf("ShardSQL has meta-var column %q = %v, want %v: %s", col, got, want, plan.ShardSQL)
				}
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// A bare identifier that ISN'T a reserved meta-variable name must not
// trigger the extra columns — only the exact reserved ALL-CAPS set
// (metaVars in planner/awk.go) does.
func TestEdge_Awk_NoMetaVarColumnsForOrdinaryVariable(t *testing.T) {
	plan, err := planBare(`cat vimgym.log | awk '{ x = $1; print x }'`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, col := range MetaVarColumns {
		if strings.Contains(plan.ShardSQL, ", "+col) {
			t.Errorf("ShardSQL unexpectedly carries meta-var column %q: %s", col, plan.ShardSQL)
		}
	}
}

func TestEdge_Jq_NonPushable(t *testing.T) {
	// jq expressions that cannot be pushed down at all.
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// Identity expression
		{
			name:        "identity",
			pipeline:    `cat vimgym.log | jq '.'`,
			wantSQL:     `SELECT CAST(payload AS VARCHAR) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`jq '.'`},
		},
		// Bare field access, object construction, and array construction all
		// project into the SELECT now — see TestRawExpr_Jq, TestRawExpr_JqObject,
		// TestRawExpr_JqArray. They live there, not here, since they ARE pushable.

		// keys/length/type
		{
			name:        "keys builtin",
			pipeline:    `cat vimgym.log | jq 'keys'`,
			wantSQL:     `SELECT CAST(payload AS VARCHAR) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`jq 'keys'`},
		},
		{
			name:        "length builtin",
			pipeline:    `cat vimgym.log | jq 'length'`,
			wantSQL:     `SELECT CAST(payload AS VARCHAR) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`jq 'length'`},
		},
		// map
		{
			name:        "map expression",
			pipeline:    `cat vimgym.log | jq 'map(.status)'`,
			wantSQL:     `SELECT CAST(payload AS VARCHAR) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`jq 'map(.status)'`},
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

func TestEdge_Jq_Pushable(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

func TestEdge_Jq_PartialPushdown(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// select | field access
		{
			name:        "select then field access",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .payload'`,
			wantSQL:     "SELECT " + leaf(".payload") + ` as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: nil,
		},
		// select | select | object construction — both selects push as
		// predicates, and the trailing object construction folds into a
		// json_object SELECT (metavar $ts maps to the ts column).
		{
			name:        "two selects then construction",
			pipeline:    `cat vimgym.log | jq 'select($level == "ERROR") | select(.status == 500) | {ts: $ts}'`,
			wantSQL:     "SELECT " + jqObj("ts", "COALESCE(CAST(to_json(CAST(ts AS VARCHAR)) AS VARCHAR), 'null')") + ` as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR' AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: nil,
		},
		// select | string-interpolation — the select pushes as a predicate
		// and the interpolation stays as a jq compute stage. Equivalent to
		// the `select | .a, .b` case in the table above; the only difference
		// is the shape of the residual. Guards against a regression where
		// formatJqExpr didn't know how to round-trip StringInterp nodes and
		// bailed on the whole pushdown.
		{
			name:        "select then string interpolation",
			pipeline:    `cat vimgym.log | jq 'select(.type == "disk") | "\(.host) \(.used_gb)"'`,
			wantSQL:     "SELECT " + render(narrowedPayloadSQL([]string{"host", "type", "used_gb"}), dialect.DuckDB) + ` as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.type'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.type') = 'disk'))`,
			wantCompute: []string{`jq 'select(.type == "disk") | "\(.host) \(.used_gb)"'`},
		},
		// -r flag must not prevent predicate pushdown, and must be preserved
		// on any residual compute stage so raw-output semantics stick.
		{
			name:        "select with -r, full pushdown",
			pipeline:    `cat vimgym.log | jq -r 'select(.status == 500)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: nil,
		},
		{
			name:        "select with -r then string interpolation",
			pipeline:    `cat vimgym.log | jq -r 'select(.type == "disk") | "\(.host) \(.used_gb)"'`,
			wantSQL:     "SELECT " + render(narrowedPayloadSQL([]string{"host", "type", "used_gb"}), dialect.DuckDB) + ` as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.type'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.type') = 'disk'))`,
			wantCompute: []string{`jq -r 'select(.type == "disk") | "\(.host) \(.used_gb)"'`},
		},
		// --raw-output (long spelling) with a field projection: a string
		// prints unquoted, anything else as JSON.
		{
			name:     "select with --raw-output long spelling",
			pipeline: `cat vimgym.log | jq --raw-output 'select(.status == 500) | .payload'`,
			wantSQL:  "SELECT " + render(jqPrintPathSQL(jqPath{".payload"}, true), dialect.DuckDB) + ` as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Grep edge cases
// ---------------------------------------------------------------------------

func TestEdge_Grep(t *testing.T) {
	t.Run("non-pushable flags", func(t *testing.T) {
		// Flags that we don't know how to push down — entire stage goes to compute.
		tests := []struct {
			name        string
			pipeline    string
			wantSQL     string
			wantCompute []string
		}{
			{
				name:        "grep -n line numbers",
				pipeline:    `cat vimgym.log | grep -n ERROR`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`grep -n ERROR`},
			},
			{
				name:        "grep -w word match",
				pipeline:    `cat vimgym.log | grep -w error`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`grep -w error`},
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
					t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
						len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
				}
				for i, want := range tt.wantCompute {
					if plan.ComputeStages[i] != want {
						t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
					}
				}
			})
		}
	})
}

// -E changes nothing for the grep that runs the stage (every pattern is RE2
// there), so it pushes as any pattern does.
func TestEdge_Grep_ExtendedRegexPushes(t *testing.T) {
	for _, tt := range []struct{ pipeline, where string }{
		{`cat vimgym.log | grep -E 'error|warn'`, `regexp_matches(raw, 'error|warn')`},
		{`cat vimgym.log | grep -Ei 'error|warn'`, `regexp_matches(raw, 'error|warn', 'i')`},
		{`cat vimgym.log | grep -Ev 'health|ping'`, `NOT regexp_matches(raw, 'health|ping')`},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if want := "WHERE namespace = ? AND " + tt.where; !strings.Contains(plan.ShardSQL, want) {
				t.Errorf("ShardSQL = %s, want it to contain %s", plan.ShardSQL, want)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("compute stages = %v, want none", plan.ComputeStages)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Sed edge cases
// ---------------------------------------------------------------------------

// An address-only sed program that selects lines is a filter, whichever of
// -n/p, d and ! spells it.
func TestEdge_SedAddressFilters(t *testing.T) {
	const prefix = `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND `
	tests := []struct{ pipeline, pred string }{
		{`sed -n '/pattern/p'`, `regexp_matches(raw, 'pattern')`},
		{`sed -n '/pattern/!p'`, `NOT regexp_matches(raw, 'pattern')`},
		{`sed '/pattern/d'`, `NOT regexp_matches(raw, 'pattern')`},
		{`sed '/pattern/!d'`, `regexp_matches(raw, 'pattern')`},
	}
	for _, tt := range tests {
		t.Run(tt.pipeline, func(t *testing.T) {
			plan, err := planBare(`cat vimgym.log | `+tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.ShardSQL != prefix+tt.pred {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, prefix+tt.pred)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("ComputeStages = %v, want none", plan.ComputeStages)
			}
		})
	}
}

func TestEdge_Sed(t *testing.T) {
	t.Run("non-pushable", func(t *testing.T) {
		tests := []struct {
			name        string
			pipeline    string
			wantSQL     string
			wantCompute []string
		}{
			// Substitution without -n is not a filter
			{
				name:        "substitution without -n",
				pipeline:    `cat vimgym.log | sed 's/error/ERROR/g'`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`sed 's/error/ERROR/g'`},
			},
			// -n with delete instead of print
			{
				name:        "-n with delete command",
				pipeline:    `cat vimgym.log | sed -n '/pattern/d'`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`sed -n '/pattern/d'`},
			},
			// No -n flag with address print (prints everything plus matching lines twice)
			{
				name:        "address print without -n",
				pipeline:    `cat vimgym.log | sed '/timeout/p'`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`sed '/timeout/p'`},
			},
			// Range address
			{
				name:        "range address",
				pipeline:    `cat vimgym.log | sed -n '/start/,/end/p'`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`sed -n '/start/,/end/p'`},
			},
			// Multiple commands
			{
				name:        "multiple commands with semicolon",
				pipeline:    `cat vimgym.log | sed -n '/error/p; /warn/p'`,
				wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
				wantCompute: []string{`sed -n '/error/p; /warn/p'`},
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
					t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
						len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
				}
				for i, want := range tt.wantCompute {
					if plan.ComputeStages[i] != want {
						t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
					}
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// SQL special character escaping
// ---------------------------------------------------------------------------

func TestEdge_SQLEscaping(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// Single quote in string literal
		{
			name:     "awk string with single quote",
			pipeline: `cat vimgym.log | awk 'LEVEL == "can'"'"'t"'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'can''t'`,
		},
		// SQL specials previously escaped by the LIKE-substring path
		// (the body wrapped `%pat%` so `%` and `_` had to be \-escaped).
		// Under the regex `~` operator those are plain literals — `%`
		// and `_` mean themselves in regex, no escaping needed.
		{
			name:     "grep pattern with percent",
			pipeline: `cat vimgym.log | grep 100%`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, '100%')`,
		},
		{
			name:     "grep pattern with underscore",
			pipeline: `cat vimgym.log | grep user_id`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'user_id')`,
		},
		{
			name:     "awk regex with underscore",
			pipeline: `cat vimgym.log | awk '/user_name/'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'user_name')`,
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
		})
	}
}

// ---------------------------------------------------------------------------
// Pipeline ordering and interaction edge cases
// ---------------------------------------------------------------------------

func TestEdge_PipelineOrdering(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// head in the middle after a filter — should merge into pushdown
		{
			name:        "filter then head in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | head -5`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR') ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 5`,
			wantCompute: nil,
		},
		// tail in the middle — should merge. Wrapped in outer ASC for
		// chronological display (the user-facing ORDER BY).
		{
			name:        "filter then tail in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | tail -5`,
			wantSQL:     `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR') ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 5) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantCompute: nil,
		},
		// head as source then awk meta-var equality: the line count
		// freezes the LIMIT, so the filter runs in compute on the first
		// 50 rows ("the first 50 rows, then keep LEVEL=ERROR"), and the
		// shard SQL is only the limit scan with the meta-var columns
		// compute needs. See TestPushdown_HardLimitBlocksPredicatePushdown.
		{
			name:        "head source then awk filter",
			pipeline:    `head -50 vimgym.log | awk 'LEVEL == "ERROR"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, level FROM logs WHERE namespace = ? ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 50`,
			wantCompute: []string{`awk 'LEVEL == "ERROR"'`},
		},
		// -n flag in mid-pipeline position
		{
			name:        "filter then tail -n in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | tail -n 20`,
			wantSQL:     `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR') ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 20) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantCompute: nil,
		},
		// All compute — no pushable stages at all
		{
			name:     "cat then sort then uniq",
			pipeline: `cat vimgym.log | sort | uniq -c`,
			wantSQL:  `SELECT raw, min(ts) AS ts, count(*) AS count FROM (SELECT unnest(string_split(raw, chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?) AS t GROUP BY raw ORDER BY raw`,
		},
		// Five-stage mixed pipeline
		{
			name:        "five stage mixed",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR"' | grep timeout | sort | uniq -c | head -10`,
			wantSQL:     `SELECT raw, min(ts) AS ts, count(*) AS count FROM (SELECT unnest(string_split(raw, chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR' AND regexp_matches(raw, 'timeout')) AS t GROUP BY raw ORDER BY raw`,
			wantCompute: []string{"head -10"},
		},
		// Partial awk pushdown followed by more compute
		{
			name:        "partial awk then sort",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR" { print $3 }' | sort`,
			wantSQL:     `SELECT COALESCE(list_extract(fields, 3), '') as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{`sort`},
		},
		// Partial jq pushdown followed by more compute — .payload folds into the
		// SELECT, leaving only sort to run downstream.
		{
			name:        "partial jq then sort",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .payload' | sort`,
			wantSQL:     "SELECT " + leaf(".payload") + ` as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: []string{`sort`},
		},
		// wc without -l flag — not pushable
		{
			name:        "wc without flags",
			pipeline:    `cat vimgym.log | grep ERROR | wc`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR')`,
			wantCompute: []string{"wc"},
		},
		// sort with flags — still compute
		{
			name:        "sort with flags",
			pipeline:    `cat vimgym.log | grep ERROR | sort -rn`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR')`,
			wantCompute: []string{"sort -rn"},
		},
		// uniq without -c — still compute
		{
			name:     "uniq without flags",
			pipeline: `cat vimgym.log | grep ERROR | sort | uniq`,
			wantSQL:  `SELECT raw, min(ts) AS ts, count(*) AS count FROM (SELECT unnest(string_split(raw, chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'ERROR')) AS t GROUP BY raw ORDER BY raw`,
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cross-tool interaction: mixing tools in a single pipeline
// ---------------------------------------------------------------------------

func TestEdge_CrossTool(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// jq select pushes, then awk print is compute
		{
			name:        "jq select then awk print",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500)' | awk '{ print $1 }'`,
			wantSQL:     `SELECT COALESCE(list_extract(fields, 1), '') as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: nil,
		},
		// grep then jq select then sort
		{
			name:        "grep then jq select then sort",
			pipeline:    `cat vimgym.log | grep error | jq 'select(.status == 500)' | sort`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'error') AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: []string{`sort`},
		},
		// awk filter then jq select — both push
		{
			name:        "awk filter then jq select",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR"' | jq 'select(.status == 500)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR' AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500))`,
			wantCompute: nil,
		},
		// sed filter then grep — both push
		{
			name:        "sed filter then grep",
			pipeline:    `cat vimgym.log | sed -n '/timeout/p' | grep ERROR`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout') AND regexp_matches(raw, 'ERROR')`,
			wantCompute: nil,
		},
		// Four different tools all pushing
		{
			name:        "four tools all pushable",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR"' | grep timeout | jq 'select(.status == 500)' | sed -n '/fatal/p'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR' AND regexp_matches(raw, 'timeout') AND (COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500)) AND regexp_matches(raw, 'fatal')`,
			wantCompute: nil,
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NR edge cases
// ---------------------------------------------------------------------------

func TestEdge_NR(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// Single NR bound (not a range) — should NOT push as LIMIT/OFFSET,
		// because NR >= 10 alone means "skip first 9" which needs the whole
		// stream. We only handle closed ranges NR >= X && NR <= Y.
		{
			name:        "NR lower bound only",
			pipeline:    `cat vimgym.log | awk 'NR >= 10'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk 'NR >= 10'`},
		},
		{
			name:        "NR upper bound only",
			pipeline:    `cat vimgym.log | awk 'NR <= 50'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk 'NR <= 50'`},
		},
		// NR range starting at 1 — equivalent to head
		{
			name:     "NR range from 1",
			pipeline: `cat vimgym.log | awk 'NR >= 1 && NR <= 20'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? LIMIT 20`,
		},
		// NR mixed with regular predicate — can't combine
		{
			name:     "NR mixed with filter",
			pipeline: `cat vimgym.log | awk 'NR >= 10 && LEVEL == "ERROR"'`,
			// LEVEL remains in the compute-stage-evaluated filter (NR
			// mixing blocks full pushdown), so it needs the real
			// per-row value the same way an output read would.
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, level FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk 'NR >= 10 && LEVEL == "ERROR"'`},
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Warp.log (all-apps) with filters
// ---------------------------------------------------------------------------

func TestEdge_WarpLog(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		{
			name:     "warp.log with awk app filter",
			pipeline: `cat warp.log | awk 'NAMESPACE == "vimgym" && LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = 'vimgym' AND level = 'ERROR'`,
		},
		{
			name:     "warp.log with grep",
			pipeline: `cat warp.log | grep timeout`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE regexp_matches(raw, 'timeout')`,
		},
		{
			name:        "warp.log with jq meta app filter",
			pipeline:    `cat warp.log | jq 'select($namespace == "callbox")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = 'callbox'`,
			wantCompute: nil,
		},
		{
			name:        "warp.log head with sort",
			pipeline:    `head warp.log | sort`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST LIMIT 10`,
			wantCompute: []string{"sort"},
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Projection safety: awk print must NOT be pushed down when a preceding
// compute stage transforms the data. The shard's `fields` column would not
// match the modified row layout.
// ---------------------------------------------------------------------------

func TestEdge_ProjectionSafety(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// sort is a non-projectable compute stage before awk;
		// awk after sort must stay in compute.
		{
			name:        "sort then awk print",
			pipeline:    `cat vimgym.log | sort | awk '{print $2}'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{"sort", `awk '{print $2}'`},
		},
		// sed modifies field contents — awk's $3 is on transformed text.
		{
			name:        "sed then awk print",
			pipeline:    `cat vimgym.log | sed 's/GET/POST/' | awk '{print $3}'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`sed 's/GET/POST/'`, `awk '{print $3}'`},
		},
		// uniq -c prepends a count column — completely changes field layout.
		{
			name:        "uniq -c then awk print",
			pipeline:    `cat vimgym.log | sort | uniq -c | awk '{print $1}'`,
			wantSQL:     `SELECT raw, min(ts) AS ts, count(*) AS count FROM (SELECT unnest(string_split(raw, chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?) AS t GROUP BY raw ORDER BY raw`,
			wantCompute: []string{`awk '{print $1}'`},
		},
		// Partial pushdown with printf remainder — printf transforms data,
		// so the following awk must stay in compute.
		{
			name:        "partial printf then awk print",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR" { printf "%s=%s\n", $1, $2 }' | awk '{print $1}'`,
			wantSQL:     plainSQL(`namespace = ? AND level = 'ERROR'`, "level"),
			wantCompute: []string{`awk 'LEVEL == "ERROR" { printf "%s=%s\n", $1, $2 }'`, `awk '{print $1}'`},
		},
		// END block produces entirely different output — cannot project past it.
		{
			name:        "END block then awk print",
			pipeline:    `cat vimgym.log | awk '/error/ { c++ } END { print c }' | awk '{print $1}'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'error')`,
			wantCompute: []string{`awk '/error/ { c++ } END { print c }'`, `awk '{print $1}'`},
		},
		// Two awk prints chained — first is projected, second must compute
		// on the projected data (its field refs are relative to the new row).
		{
			name:        "awk print chain",
			pipeline:    `cat vimgym.log | awk '{print $3, $4}' | awk '{print $1}'`,
			wantSQL:     `SELECT concat_ws(' ', COALESCE(list_extract(fields, 3), ''), COALESCE(list_extract(fields, 4), '')) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk '{print $1}'`},
		},
		// Partial pushdown with simple print + sort — print projects, sort computes.
		// This IS safe because the print is the first compute operation.
		{
			name:        "partial print then sort is safe",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR" { print $3 }' | sort`,
			wantSQL:     `SELECT COALESCE(list_extract(fields, 3), '') as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{`sort`},
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
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SQL-injection regressions. User-derived bytes that land inside a SQL
// string literal must be quote-escaped so the literal can't be closed
// early. Every case here must also produce a customer-predicate-led
// WHERE clause when Plan is given a customer (the row-level security
// boundary lives at the head of the inner SELECT regardless of any
// outer LIMIT-bound subquery shape).
// ---------------------------------------------------------------------------

func TestEdge_SQLInjection(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "grep pattern with single quote",
			pipeline: `cat foo.log | grep "x' OR 1=1 --"`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'x'' OR 1=1 --')`,
		},
		{
			name:     "sed address with single quote",
			pipeline: `cat foo.log | sed -n "/x' OR 1=1 --/p"`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'x'' OR 1=1 --')`,
		},
		{
			name:     "awk regex with single quote",
			pipeline: `cat foo.log | awk '/x'"'"' OR 1=1/'`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND regexp_matches(raw, 'x'' OR 1=1')`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL mismatch\n  got:  %s\n  want: %s", plan.ShardSQL, tt.wantSQL)
			}
			// Re-plan with a customer; the customer predicate must lead
			// the inner WHERE clause (immediately after "WHERE ") so
			// LIMIT-bound subqueries stay group-scoped before the
			// outer wrapper applies any cross-shard merge.
			withCust, err := Plan(tt.pipeline, shapetest.Group(3), "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q) with customer: %v", tt.pipeline, err)
			}
			if !strings.Contains(withCust.ShardSQL, "WHERE "+shapetest.Predicate()) {
				t.Errorf("customer predicate not AND-dominant after Plan with customer:\n  %s", withCust.ShardSQL)
			}
		})
	}
}
