package planner

import (
	"strings"
	"testing"
	"time"
)

// These tests describe jq projection optimizations: object construction,
// array construction, nested-field paths, and array indexing all collapse
// into shard-side SQL, parallel to the single-field optimization in
// TestRawExpr_Jq.
//
// We pin the SQL with strings.Contains and check ComputeStages exactly so
// we catch both halves of the win: the right SQL shape AND the dropped
// jq stage.
//
// jqProj wraps the inner SQL projection with the VARCHAR cast the planner
// emits so downstream jq stages (and the Go driver) see consistent text.
func jqProj(inner string) string {
	return "CAST(" + inner + " AS VARCHAR) as raw"
}

// TestRawExpr_JqNestedField — pure path access composes into a deep $.a.b.c
// extract, regardless of whether it was written `.a.b.c` or `.a | .b | .c`
// (warp parses both to the same PipeExpr-of-fields AST).
func TestRawExpr_JqNestedField(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantInSQL string
	}{
		{
			name:      "two-level dotted",
			pipeline:  `cat vimgym.log | jq '.foo.bar'`,
			wantInSQL: jqProj("json_extract(payload, '$.foo.bar')"),
		},
		{
			name:      "three-level dotted",
			pipeline:  `cat vimgym.log | jq '.a.b.c'`,
			wantInSQL: jqProj("json_extract(payload, '$.a.b.c')"),
		},
		{
			name:      "pipe spelling of nested",
			pipeline:  `cat vimgym.log | jq '.foo | .bar'`,
			wantInSQL: jqProj("json_extract(payload, '$.foo.bar')"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
				t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// TestRawExpr_JqArrayIndex — `.items[0]` parses as PipeExpr{FieldExpr, IndexExpr}
// and should collapse to a JSONPath array index in the SELECT.
func TestRawExpr_JqArrayIndex(t *testing.T) {
	plan, err := Plan(`cat vimgym.log | jq '.items[0]'`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := jqProj("json_extract(payload, '$.items[0]')")
	if !strings.Contains(plan.ShardSQL, want) {
		t.Errorf("ShardSQL missing %q\n got: %s", want, plan.ShardSQL)
	}
	if len(plan.ComputeStages) != 0 {
		t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
	}
}

// TestRawExpr_JqObject — object construction over field refs collapses into
// a json_object(...) call. The resulting `raw` is a JSON object (parsable by
// jq if more stages came after), and the jq stage drops out.
func TestRawExpr_JqObject(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantInSQL string
	}{
		{
			name:      "single-key object",
			pipeline:  `cat vimgym.log | jq '{user: .user_id}'`,
			wantInSQL: jqProj("json_object('user', json_extract(payload, '$.user_id'))"),
		},
		{
			name:      "two-key object",
			pipeline:  `cat vimgym.log | jq '{user: .user_id, action: .action}'`,
			wantInSQL: jqProj("json_object('user', json_extract(payload, '$.user_id'), 'action', json_extract(payload, '$.action'))"),
		},
		{
			name: "shorthand object expands to full form",
			// `{user_id, action}` is sugar for `{user_id: .user_id, action: .action}`
			// — warp's parser already normalizes it, so the output should match
			// the explicit form.
			pipeline:  `cat vimgym.log | jq '{user_id, action}'`,
			wantInSQL: jqProj("json_object('user_id', json_extract(payload, '$.user_id'), 'action', json_extract(payload, '$.action'))"),
		},
		{
			name:      "object with string literal value",
			pipeline:  `cat vimgym.log | jq '{type: "log", value: .pct}'`,
			wantInSQL: jqProj("json_object('type', 'log', 'value', json_extract(payload, '$.pct'))"),
		},
		{
			name:      "object with metavar value",
			pipeline:  `cat vimgym.log | jq '{when: ._ts, who: .user_id}'`,
			wantInSQL: jqProj("json_object('when', ts, 'who', json_extract(payload, '$.user_id'))"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
				t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// TestRawExpr_JqArray — `[.x, .y]` parses as ArrayExpr{Elems: CommaExpr{...}}
// and should collapse to json_array(...).
func TestRawExpr_JqArray(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantInSQL string
	}{
		{
			name:      "two-elem field array",
			pipeline:  `cat vimgym.log | jq '[.user_id, .action]'`,
			wantInSQL: jqProj("json_array(json_extract(payload, '$.user_id'), json_extract(payload, '$.action'))"),
		},
		{
			name:      "array with literal",
			pipeline:  `cat vimgym.log | jq '[.user_id, "click"]'`,
			wantInSQL: jqProj("json_array(json_extract(payload, '$.user_id'), 'click')"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
				t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// TestRawExpr_JqInternalPipeBreak — when a jq expression is a pipe whose
// PREFIX is pushable (path access, optionally after row-level selects) but
// whose SUFFIX is not (length, sort_by, select-against-the-value, iter, etc.),
// we should push the prefix into SELECT and ship only the unpushable tail
// to compute. Today the planner takes one of two wrong actions: it either
// (a) refuses to push anything because the whole expression isn't a single
// FieldExpr, or (b) for select-after-path, would incorrectly push the
// inner select as a row-level WHERE — `.foo | select(.bar==1)` filters by
// `.foo.bar==1`, NOT `.bar==1`.
func TestRawExpr_JqInternalPipeBreak(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantInSQL   string
		wantCompute []string
		// If notPushable is true, the prefix can't be a SELECT projection
		// either (e.g. `.items[]` destructures one row into many) — the
		// whole jq stays in compute and raw stays "raw".
		notPushable bool
	}{
		{
			name:        "field then length",
			pipeline:    `cat vimgym.log | jq '.foo | length'`,
			wantInSQL:   jqProj("json_extract(payload, '$.foo')"),
			wantCompute: []string{"jq 'length'"},
		},
		{
			name:        "field then tostring",
			pipeline:    `cat vimgym.log | jq '.tags | tostring'`,
			wantInSQL:   jqProj("json_extract(payload, '$.tags')"),
			wantCompute: []string{"jq 'tostring'"},
		},
		{
			name:        "row-select then field then sort_by",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .users | sort_by(.name)'`,
			wantInSQL:   jqProj("json_extract(payload, '$.users')"),
			wantCompute: []string{"jq 'sort_by(.name)'"},
		},
		{
			name: "select after path is value-relative — must terminate before it",
			// .foo | select(.bar == 1) means "keep rows where foo.bar == 1
			// and emit foo". The select operates on the value of .foo, so
			// it CANNOT be lifted to a row-level WHERE on $.bar. Safe move:
			// project .foo, run the select in compute against that value.
			pipeline:    `cat vimgym.log | jq '.foo | select(.bar == 1)'`,
			wantInSQL:   jqProj("json_extract(payload, '$.foo')"),
			wantCompute: []string{`jq 'select(.bar == 1)'`},
		},
		{
			name: "iter — array projects, .[] runs in compute",
			// .items[] is PipeExpr{FieldExpr(items), IterExpr}. The path
			// prefix `.items` is pushable; the residual IterExpr renders as
			// `.[]` (valid jq after a pipe). Net effect: the shard ships
			// just the array, the compute jq fans it out into per-element
			// values — same outputs as running `jq '.items[]'` on the full
			// payload.
			pipeline:    `cat vimgym.log | jq '.items[]'`,
			wantInSQL:   jqProj("json_extract(payload, '$.items')"),
			wantCompute: []string{`jq '.[]'`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if tt.notPushable {
				if !strings.Contains(plan.ShardSQL, "SELECT raw, ts FROM logs") {
					t.Errorf("expected un-projected SELECT, got: %s", plan.ShardSQL)
				}
			} else {
				if !strings.Contains(plan.ShardSQL, tt.wantInSQL) {
					t.Errorf("ShardSQL missing %q\n got: %s", tt.wantInSQL, plan.ShardSQL)
				}
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages count: got %d %v, want %d %v",
					len(plan.ComputeStages), plan.ComputeStages,
					len(tt.wantCompute), tt.wantCompute)
			}
			for i, w := range tt.wantCompute {
				if plan.ComputeStages[i] != w {
					t.Errorf("ComputeStages[%d]: got %q want %q", i, plan.ComputeStages[i], w)
				}
			}
		})
	}
}
