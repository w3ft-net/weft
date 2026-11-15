package planner

import (
	"testing"

	"github.com/graybeardcorp/warp/pkg/jq"
)

// TestJqFrontend_StringInterp pins that a select(...) prefix lowers to
// a prefilter whatever follows it, here string interpolation. It used
// to bail when the tail couldn't be re-rendered (the user-visible bug:
// `explain` showed ShardSQL without the select); the tail is never
// re-rendered now, since the stage runs whole in compute.
func TestJqFrontend_StringInterp(t *testing.T) {
	tests := []struct {
		name          string
		expression    string
		wantPredicate string
	}{
		{
			name:          "two interpolations, literal in between",
			expression:    `select(.type == "disk") | "\(.host) \(.used_gb)"`,
			wantPredicate: `(COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.type'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.type') = 'disk'))`,
		},
		{
			name:          "leading literal then interpolation",
			expression:    `select(.type == "disk") | "host: \(.host)"`,
			wantPredicate: `(COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.type'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.type') = 'disk'))`,
		},
		{
			name:          "single interpolation, no literals",
			expression:    `select(.type == "disk") | "\(.host)"`,
			wantPredicate: `(COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT') AND (COALESCE(json_type(payload, '$.type'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.type') = 'disk'))`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Sanity: make sure warp parses the input at all. If this
			// fails, the bug is upstream (warp's parser, not our formatter)
			// and the fix belongs in warp/pkg/jq.
			if expr, err := jq.Parse(tt.expression); err != nil {
				t.Fatalf("jq.Parse(%q): %v", tt.expression, err)
			} else {
				t.Logf("parsed as %T", expr)
			}
			ls := lowerStage(stageInfo{cmd: "jq", args: []string{tt.expression}}, 1, false)
			if len(ls.nodes) != 2 {
				t.Fatalf("want a prefilter and an opaque stage, got %#v", ls.nodes)
			}
			f, ok := ls.nodes[0].(*filterNode)
			if !ok || f.exact {
				t.Fatalf("a jq select before a tail is a prefilter, got %#v", ls.nodes[0])
			}
			if f.pred != tt.wantPredicate {
				t.Errorf("predicate\n got: %s\nwant: %s", f.pred, tt.wantPredicate)
			}
			if op, ok := ls.nodes[1].(*opaqueNode); !ok || op.why.code != reasonJqTail {
				t.Errorf("the stage must still run, opaque for its tail: %#v", ls.nodes[1])
			}
		})
	}
}
