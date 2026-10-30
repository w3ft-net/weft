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
// jqProj is the projection column the planner emits for the SQL that writes
// the text jq prints.
func jqProj(inner string) string { return inner + " as raw" }

// leaf is the SQL for the text jq prints for the value at a payload path,
// given as its steps (".foo", ".bar", "[0]").
func leaf(steps ...string) string { return jqLeafSQL(jqPath(steps)) }

// jqObj and jqArr are the SQL that writes an object or array out as text,
// from the SQL for each value (an object's as key, value pairs).
func jqObj(pairs ...string) string {
	var parts []string
	for i := 0; i < len(pairs); i += 2 {
		parts = append(parts, "'"+jqStringJSON(pairs[i])+":' || "+pairs[i+1])
	}
	return "'{' || " + strings.Join(parts, " || ',' || ") + " || '}'"
}

func jqArr(values ...string) string {
	return "'[' || " + strings.Join(values, " || ',' || ") + " || ']'"
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
			wantInSQL: jqProj(leaf(".foo", ".bar")),
		},
		{
			name:      "three-level dotted",
			pipeline:  `cat vimgym.log | jq '.a.b.c'`,
			wantInSQL: jqProj(leaf(".a", ".b", ".c")),
		},
		{
			name:      "pipe spelling of nested",
			pipeline:  `cat vimgym.log | jq '.foo | .bar'`,
			wantInSQL: jqProj(leaf(".foo", ".bar")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
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
	plan, err := Plan(`cat vimgym.log | jq '.items[0]'`, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := jqProj(leaf(".items", "[0]"))
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
			wantInSQL: jqProj(jqObj("user", leaf(".user_id"))),
		},
		{
			name:      "two-key object",
			pipeline:  `cat vimgym.log | jq '{user: .user_id, action: .action}'`,
			wantInSQL: jqProj(jqObj("user", leaf(".user_id"), "action", leaf(".action"))),
		},
		{
			name: "shorthand object expands to full form",
			// `{user_id, action}` is sugar for `{user_id: .user_id, action: .action}`
			// — warp's parser already normalizes it, so the output should match
			// the explicit form.
			pipeline:  `cat vimgym.log | jq '{user_id, action}'`,
			wantInSQL: jqProj(jqObj("user_id", leaf(".user_id"), "action", leaf(".action"))),
		},
		{
			name:      "object with string literal value",
			pipeline:  `cat vimgym.log | jq '{type: "log", value: .pct}'`,
			wantInSQL: jqProj(jqObj("type", `'"log"'`, "value", leaf(".pct"))),
		},
		{
			name:      "object with metavar value",
			pipeline:  `cat vimgym.log | jq '{when: $ts, who: .user_id}'`,
			wantInSQL: jqProj(jqObj("when", "COALESCE(CAST(to_json(CAST(ts AS VARCHAR)) AS VARCHAR), 'null')", "who", leaf(".user_id"))),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
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
			wantInSQL: jqProj(jqArr(leaf(".user_id"), leaf(".action"))),
		},
		{
			name:      "array with literal",
			pipeline:  `cat vimgym.log | jq '[.user_id, "click"]'`,
			wantInSQL: jqProj(jqArr(leaf(".user_id"), `'"click"'`)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
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

// TestRawExpr_JqInternalPipeBreak pins that a jq program whose head is
// a path but whose tail isn't SQL runs whole, as written, over the
// row's payload (see docs/plans/planner-ir.md, "Never regenerate source").
func TestRawExpr_JqInternalPipeBreak(t *testing.T) {
	for pipeline, key := range map[string]string{
		`cat vimgym.log | jq '.foo | length'`:    "foo",
		`cat vimgym.log | jq '.tags | tostring'`: "tags",
		`cat vimgym.log | jq '.items[]'`:         "items",
	} {
		t.Run(pipeline, func(t *testing.T) {
			plan, err := Plan(pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			// The stage reads the payload, not raw: just the key it reads.
			if want := "SELECT " + narrowedPayloadSQL([]string{key}) + " as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs"; !strings.HasPrefix(plan.ShardSQL, want) {
				t.Errorf("expected the payload's %q as the stage's input, got: %s", key, plan.ShardSQL)
			}
			stage := strings.TrimPrefix(pipeline, "cat vimgym.log | ")
			if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != stage {
				t.Errorf("compute = %q, want [%q]", plan.ComputeStages, stage)
			}
		})
	}
}
