package planner

import (
	"errors"
	"strings"
	"testing"
	"time"
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
				_, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
				if err == nil {
					t.Errorf("Plan(%q): expected error, got nil", tt.pipeline)
				}
			})
		}
	})

	t.Run("local pipeline", func(t *testing.T) {
		_, err := Plan("echo 42", "", time.Time{}, time.Time{})
		if !errors.Is(err, ErrNotPlannable) {
			t.Fatalf("Plan(\"echo 42\") err = %v, want ErrNotPlannable", err)
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
				wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'callbox' OR app LIKE 'callbox/%')`,
			},
			{
				name:     "hyphenated app name",
				pipeline: `cat my-service.log`,
				wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'my-service' OR app LIKE 'my-service/%')`,
			},
			{
				name:     "underscored app name",
				pipeline: `cat my_service.log`,
				wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'my_service' OR app LIKE 'my_service/%')`,
			},
			{
				name:     "head on warp.log",
				pipeline: `head warp.log`,
				wantSQL:  `SELECT raw, ts FROM logs ORDER BY ts ASC LIMIT 10`,
			},
			{
				name:     "tail on warp.log",
				pipeline: `tail -50 warp.log`,
				wantSQL:  `SELECT raw, ts FROM logs ORDER BY ts DESC LIMIT 50`,
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
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk 'BEGIN { print "header" }'`},
		},
		// No pattern at all, just an action on every line
		{
			name:        "unconditional print action",
			pipeline:    `cat vimgym.log | awk '{ print $2 }'`,
			wantSQL:     `SELECT list_extract(fields, 2) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: nil, // simple $N print fully pushed down as projection
		},
		// User-defined function
		{
			name:        "program with function definition",
			pipeline:    `cat vimgym.log | awk 'function foo() { return 1 } /test/'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk 'function foo() { return 1 } /test/'`},
		},
		// Assignment in pattern position — not a filter
		{
			name:        "assignment expression is not a filter",
			pipeline:    `cat vimgym.log | awk '{ x = $3 + 1; print x }'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk '{ x = $3 + 1; print x }'`},
		},
		// Non-pushable field access: $NF (variable index, not a literal)
		{
			name:        "variable field index NF",
			pipeline:    `cat vimgym.log | awk '$NF == "ERROR"'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk '$NF == "ERROR"'`},
		},
		// Field compared to another field — can't push down
		{
			name:        "field vs field comparison",
			pipeline:    `cat vimgym.log | awk '$3 > $4'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk '$3 > $4'`},
		},
		// Function call in pattern — can't push down
		{
			name:        "function call in pattern",
			pipeline:    `cat vimgym.log | awk 'length($0) > 100'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk 'length($0) > 100'`},
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
//	tail /log/weft/coordinator.log | awk '$4 == "compute" {print}'
//
// Two facts collide here. (1) After tail freezes the LIMIT, every
// subsequent stage MUST run in compute — pushing a filter shard-side
// post-LIMIT lets cross-shard merge re-saturate the result (see
// docs/plans/cross-shard-post-limit-filter.md). (2) extractRawExpr's
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
			pipeline: `tail /log/weft/coordinator.log | awk '$4 == "compute" {print}'`,
		},
		{
			name:     "head then awk positional equality with explicit print",
			pipeline: `head /log/weft/coordinator.log | awk '$3 == "compute" {print}'`,
		},
		{
			name:     "tail then awk positional equality with default action",
			pipeline: `tail /log/weft/coordinator.log | awk '$4 == "compute"'`,
		},
		{
			name:     "tail then awk regex on $0 with explicit print",
			pipeline: `tail /log/weft/coordinator.log | awk '$0 ~ /compute/ {print}'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
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
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND raw = 'exact match'`,
		},
		// Deeply nested boolean logic
		{
			name:     "nested AND inside OR",
			pipeline: `cat vimgym.log | awk '($LEVEL == "ERROR" && $3 > 100) || $LEVEL == "FATAL"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND ((level = 'ERROR' AND CAST(list_extract(fields, 3) AS DOUBLE) > 100) OR level = 'FATAL')`,
		},
		// Large positional field number
		{
			name:     "high positional field",
			pipeline: `cat vimgym.log | awk '$15 == "value"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND list_extract(fields, 15) = 'value'`,
		},
		// Field 1
		{
			name:     "field 1",
			pipeline: `cat vimgym.log | awk '$1 == "2026-03-28"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND list_extract(fields, 1) = '2026-03-28'`,
		},
		// Numeric equality (not just comparison)
		{
			name:     "field numeric equality",
			pipeline: `cat vimgym.log | awk '$3 == 200'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(list_extract(fields, 3) AS DOUBLE) = 200`,
		},
		// Numeric not-equal
		{
			name:     "field numeric not equal",
			pipeline: `cat vimgym.log | awk '$3 != 0'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(list_extract(fields, 3) AS DOUBLE) != 0`,
		},
		// Meta-var less-than (non-standard but expressible)
		{
			name:     "meta-var less than string",
			pipeline: `cat vimgym.log | awk '$LEVEL < "WARN"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level < 'WARN'`,
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
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { printf "%s %s\n", $1, $2 }'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR'`,
			wantCompute: []string{`awk '{ printf "%s %s\n", $1, $2 }'`},
		},
		// Regex pattern + non-default action
		{
			name:        "regex pattern with print action",
			pipeline:    `cat vimgym.log | awk '/timeout/ { print $1, $2 }'`,
			wantSQL:     `SELECT concat_ws(' ', list_extract(fields, 1), list_extract(fields, 2)) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'timeout')`,
			wantCompute: nil, // simple $N print fully pushed down as projection
		},
		// Filter + increment + END (count errors)
		{
			name:        "count pattern with END",
			pipeline:    `cat vimgym.log | awk '/error/ { c++ } END { print c }'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'error')`,
			wantCompute: []string{`awk '/error/ { c++ } END { print c }'`},
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
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq '.'`},
		},
		// Bare field access, object construction, and array construction all
		// project into the SELECT now — see TestRawExpr_Jq, TestRawExpr_JqObject,
		// TestRawExpr_JqArray. They live there, not here, since they ARE pushable.

		// keys/length/type
		{
			name:        "keys builtin",
			pipeline:    `cat vimgym.log | jq 'keys'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'keys'`},
		},
		{
			name:        "length builtin",
			pipeline:    `cat vimgym.log | jq 'length'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'length'`},
		},
		// map
		{
			name:        "map expression",
			pipeline:    `cat vimgym.log | jq 'map(.status)'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'map(.status)'`},
		},
		// select with non-comparison condition (has, contains, test)
		{
			name:        "select with has",
			pipeline:    `cat vimgym.log | jq 'select(has("error"))'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'select(has("error"))'`},
		},
		// select with arithmetic in condition
		{
			name:        "select with arithmetic",
			pipeline:    `cat vimgym.log | jq 'select((.x + .y) > 100)'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'select((.x + .y) > 100)'`},
		},
		// select comparing two fields (no literal)
		{
			name:        "select comparing two fields",
			pipeline:    `cat vimgym.log | jq 'select(.start < .end)'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'select(.start < .end)'`},
		},
		// Literal on the left side of comparison
		{
			name:        "literal on left side",
			pipeline:    `cat vimgym.log | jq 'select(500 == .status)'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`jq 'select(500 == .status)'`},
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
	}{
		// Boolean field comparison
		{
			name:     "select boolean true",
			pipeline: `cat vimgym.log | jq 'select(.success == true)'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.success') AS BOOL) = true`,
		},
		// Less-than on json field
		{
			name:     "select less than",
			pipeline: `cat vimgym.log | jq 'select(.duration_ms < 100)'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.duration_ms') AS DOUBLE) < 100`,
		},
		// Greater-than-or-equal
		{
			name:     "select gte",
			pipeline: `cat vimgym.log | jq 'select(.retries >= 3)'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.retries') AS DOUBLE) >= 3`,
		},
		// Three chained selects
		{
			name:     "three chained selects",
			pipeline: `cat vimgym.log | jq 'select(._level == "ERROR") | select(.status == 500) | select(.action == "login")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR' AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND json_extract_string(payload, '$.action') = 'login'`,
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
			wantSQL:     `SELECT CAST(json_extract(payload, '$.payload') AS VARCHAR) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: nil,
		},
		// select | select | object construction — both selects push as
		// predicates, and the trailing object construction folds into a
		// json_object SELECT (metavar ._ts maps to the ts column).
		{
			name:        "two selects then construction",
			pipeline:    `cat vimgym.log | jq 'select(._level == "ERROR") | select(.status == 500) | {ts: ._ts}'`,
			wantSQL:     `SELECT CAST(json_object('ts', ts) AS VARCHAR) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
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
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND json_extract_string(payload, '$.type') = 'disk'`,
			wantCompute: []string{`jq '"\(.host) \(.used_gb)"'`},
		},
		// -r flag must not prevent predicate pushdown, and must be preserved
		// on any residual compute stage so raw-output semantics stick.
		{
			name:        "select with -r, full pushdown",
			pipeline:    `cat vimgym.log | jq -r 'select(.status == 500)'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: nil,
		},
		{
			name:        "select with -r then string interpolation",
			pipeline:    `cat vimgym.log | jq -r 'select(.type == "disk") | "\(.host) \(.used_gb)"'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND json_extract_string(payload, '$.type') = 'disk'`,
			wantCompute: []string{`jq -r '"\(.host) \(.used_gb)"'`},
		},
		// --raw-output (long spelling) with a field projection. The projection
		// must NOT fold into SELECT: json_extract returns JSON-quoted strings,
		// which would silently drop the -r semantics. Predicate still pushes;
		// the -r jq stage stays as compute to do the unquoting.
		{
			name:        "select with --raw-output long spelling",
			pipeline:    `cat vimgym.log | jq --raw-output 'select(.status == 500) | .payload'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{`jq --raw-output '.payload'`},
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
				name:        "grep -c count mode",
				pipeline:    `cat vimgym.log | grep -c ERROR`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`grep -c ERROR`},
			},
			{
				name:        "grep -n line numbers",
				pipeline:    `cat vimgym.log | grep -n ERROR`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`grep -n ERROR`},
			},
			{
				name:        "grep -E extended regex",
				pipeline:    `cat vimgym.log | grep -E 'error|warn'`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`grep -E 'error|warn'`},
			},
			{
				name:        "grep -w word match",
				pipeline:    `cat vimgym.log | grep -w error`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`grep -w error`},
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
// Sed edge cases
// ---------------------------------------------------------------------------

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
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`sed 's/error/ERROR/g'`},
			},
			// -n with delete instead of print
			{
				name:        "-n with delete command",
				pipeline:    `cat vimgym.log | sed -n '/pattern/d'`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`sed -n '/pattern/d'`},
			},
			// No -n flag with address print (prints everything plus matching lines twice)
			{
				name:        "address print without -n",
				pipeline:    `cat vimgym.log | sed '/timeout/p'`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`sed '/timeout/p'`},
			},
			// Range address
			{
				name:        "range address",
				pipeline:    `cat vimgym.log | sed -n '/start/,/end/p'`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`sed -n '/start/,/end/p'`},
			},
			// Multiple commands
			{
				name:        "multiple commands with semicolon",
				pipeline:    `cat vimgym.log | sed -n '/error/p; /warn/p'`,
				wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
				wantCompute: []string{`sed -n '/error/p; /warn/p'`},
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
			pipeline: `cat vimgym.log | awk '$LEVEL == "can'"'"'t"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'can''t'`,
		},
		// SQL specials previously escaped by the LIKE-substring path
		// (the body wrapped `%pat%` so `%` and `_` had to be \-escaped).
		// Under the regex `~` operator those are plain literals — `%`
		// and `_` mean themselves in regex, no escaping needed.
		{
			name:     "grep pattern with percent",
			pipeline: `cat vimgym.log | grep 100%`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, '100%')`,
		},
		{
			name:     "grep pattern with underscore",
			pipeline: `cat vimgym.log | grep user_id`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'user_id')`,
		},
		{
			name:     "awk regex with underscore",
			pipeline: `cat vimgym.log | awk '/user_name/'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'user_name')`,
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
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'ERROR') ORDER BY ts ASC LIMIT 5`,
			wantCompute: nil,
		},
		// tail in the middle — should merge
		{
			name:        "filter then tail in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | tail -5`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'ERROR') ORDER BY ts DESC LIMIT 5`,
			wantCompute: nil,
		},
		// head as source then awk meta-var equality — limit-safe.
		// The user's intent is "first 50 rows where LEVEL=ERROR";
		// pushing the predicate shard-side delivers that without
		// shipping the full 50-row window through compute. The
		// cross-shard saturation hazard the doc warned about applies
		// to *inclusive* filters (negation, `grep -v`); equality is
		// selective. See TestPushdown_LimitSafePushdown for the full
		// matrix and TestPushdown_LimitBeforeFilter_DoesNotMerge for
		// the hazard cases that still go to compute.
		{
			name:        "head source then awk filter",
			pipeline:    `head -50 vimgym.log | awk '$LEVEL == "ERROR"'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR' ORDER BY ts ASC LIMIT 50`,
			wantCompute: nil,
		},
		// -n flag in mid-pipeline position
		{
			name:        "filter then tail -n in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | tail -n 20`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'ERROR') ORDER BY ts DESC LIMIT 20`,
			wantCompute: nil,
		},
		// All compute — no pushable stages at all
		{
			name:        "cat then sort then uniq",
			pipeline:    `cat vimgym.log | sort | uniq -c`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{"sort", "uniq -c"},
		},
		// Five-stage mixed pipeline
		{
			name:        "five stage mixed",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep timeout | sort | uniq -c | head -10`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR' AND regexp_matches(raw, 'timeout')`,
			wantCompute: []string{"sort", "uniq -c", "head -10"},
		},
		// Partial awk pushdown followed by more compute
		{
			name:        "partial awk then sort",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { print $3 }' | sort`,
			wantSQL:     `SELECT list_extract(fields, 3) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR'`,
			wantCompute: []string{"sort"}, // simple $N print pushed down, sort remains
		},
		// Partial jq pushdown followed by more compute — .payload folds into the
		// SELECT, leaving only sort to run downstream.
		{
			name:        "partial jq then sort",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .payload' | sort`,
			wantSQL:     `SELECT CAST(json_extract(payload, '$.payload') AS VARCHAR) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{"sort"},
		},
		// wc without -l flag — not pushable
		{
			name:        "wc without flags",
			pipeline:    `cat vimgym.log | grep ERROR | wc`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'ERROR')`,
			wantCompute: []string{"wc"},
		},
		// sort with flags — still compute
		{
			name:        "sort with flags",
			pipeline:    `cat vimgym.log | grep ERROR | sort -rn`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'ERROR')`,
			wantCompute: []string{"sort -rn"},
		},
		// uniq without -c — still compute
		{
			name:        "uniq without flags",
			pipeline:    `cat vimgym.log | grep ERROR | sort | uniq`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'ERROR')`,
			wantCompute: []string{"sort", "uniq"},
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
			wantSQL:     `SELECT list_extract(fields, 1) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: nil, // simple $N print fully pushed down as projection
		},
		// grep then jq select then sort
		{
			name:        "grep then jq select then sort",
			pipeline:    `cat vimgym.log | grep error | jq 'select(.status == 500)' | sort`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'error') AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{"sort"},
		},
		// awk filter then jq select — both push
		{
			name:        "awk filter then jq select",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | jq 'select(.status == 500)'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: nil,
		},
		// sed filter then grep — both push
		{
			name:        "sed filter then grep",
			pipeline:    `cat vimgym.log | sed -n '/timeout/p' | grep ERROR`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'timeout') AND regexp_matches(raw, 'ERROR')`,
			wantCompute: nil,
		},
		// Four different tools all pushing
		{
			name:        "four tools all pushable",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep timeout | jq 'select(.status == 500)' | sed -n '/fatal/p'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR' AND regexp_matches(raw, 'timeout') AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND regexp_matches(raw, 'fatal')`,
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
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk 'NR >= 10'`},
		},
		{
			name:        "NR upper bound only",
			pipeline:    `cat vimgym.log | awk 'NR <= 50'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk 'NR <= 50'`},
		},
		// NR range starting at 1 — equivalent to head
		{
			name:     "NR range from 1",
			pipeline: `cat vimgym.log | awk 'NR >= 1 && NR <= 20'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') LIMIT 20`,
		},
		// NR mixed with regular predicate — can't combine
		{
			name:        "NR mixed with filter",
			pipeline:    `cat vimgym.log | awk 'NR >= 10 && $LEVEL == "ERROR"'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk 'NR >= 10 && $LEVEL == "ERROR"'`},
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
			pipeline: `cat warp.log | awk '$APP == "vimgym" && $LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE app = 'vimgym' AND level = 'ERROR'`,
		},
		{
			name:     "warp.log with grep",
			pipeline: `cat warp.log | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE regexp_matches(raw, 'timeout')`,
		},
		{
			name:     "warp.log with jq meta app filter",
			pipeline: `cat warp.log | jq 'select(._app == "callbox")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE app = 'callbox'`,
		},
		{
			name:        "warp.log head with sort",
			pipeline:    `head warp.log | sort`,
			wantSQL:     `SELECT raw, ts FROM logs ORDER BY ts ASC LIMIT 10`,
			wantCompute: []string{"sort"},
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
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{"sort", `awk '{print $2}'`},
		},
		// sed modifies field contents — awk's $3 is on transformed text.
		{
			name:        "sed then awk print",
			pipeline:    `cat vimgym.log | sed 's/GET/POST/' | awk '{print $3}'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`sed 's/GET/POST/'`, `awk '{print $3}'`},
		},
		// uniq -c prepends a count column — completely changes field layout.
		{
			name:        "uniq -c then awk print",
			pipeline:    `cat vimgym.log | sort | uniq -c | awk '{print $1}'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{"sort", "uniq -c", `awk '{print $1}'`},
		},
		// Partial pushdown with printf remainder — printf transforms data,
		// so the following awk must stay in compute.
		{
			name:        "partial printf then awk print",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { printf "%s=%s\n", $1, $2 }' | awk '{print $1}'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR'`,
			wantCompute: []string{`awk '{ printf "%s=%s\n", $1, $2 }'`, `awk '{print $1}'`},
		},
		// END block produces entirely different output — cannot project past it.
		{
			name:        "END block then awk print",
			pipeline:    `cat vimgym.log | awk '/error/ { c++ } END { print c }' | awk '{print $1}'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND regexp_matches(raw, 'error')`,
			wantCompute: []string{`awk '/error/ { c++ } END { print c }'`, `awk '{print $1}'`},
		},
		// Two awk prints chained — first is projected, second must compute
		// on the projected data (its field refs are relative to the new row).
		{
			name:        "awk print chain",
			pipeline:    `cat vimgym.log | awk '{print $3, $4}' | awk '{print $1}'`,
			wantSQL:     `SELECT concat_ws(' ', list_extract(fields, 3), list_extract(fields, 4)) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
			wantCompute: []string{`awk '{print $1}'`},
		},
		// Partial pushdown with simple print + sort — print projects, sort computes.
		// This IS safe because the print is the first compute operation.
		{
			name:        "partial print then sort is safe",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { print $3 }' | sort`,
			wantSQL:     `SELECT list_extract(fields, 3) as raw, ts FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%') AND level = 'ERROR'`,
			wantCompute: []string{"sort"},
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
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'foo' OR app LIKE 'foo/%') AND regexp_matches(raw, 'x'' OR 1=1 --')`,
		},
		{
			name:     "sed address with single quote",
			pipeline: `cat foo.log | sed -n "/x' OR 1=1 --/p"`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'foo' OR app LIKE 'foo/%') AND regexp_matches(raw, 'x'' OR 1=1 --')`,
		},
		{
			name:     "awk regex with single quote",
			pipeline: `cat foo.log | awk '/x'"'"' OR 1=1/'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE (app = 'foo' OR app LIKE 'foo/%') AND regexp_matches(raw, 'x'' OR 1=1')`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL mismatch\n  got:  %s\n  want: %s", plan.ShardSQL, tt.wantSQL)
			}
			// Re-plan with a customer; the customer predicate must lead
			// the inner WHERE clause (immediately after "WHERE ") so
			// LIMIT-bound subqueries stay tenant-scoped before the
			// outer wrapper applies any cross-shard merge.
			withCust, err := Plan(tt.pipeline, "victim-co", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q) with customer: %v", tt.pipeline, err)
			}
			if !strings.Contains(withCust.ShardSQL, "WHERE customer = 'victim-co' AND ") {
				t.Errorf("customer predicate not AND-dominant after Plan with customer:\n  %s", withCust.ShardSQL)
			}
		})
	}
}
