package planner

import (
	"testing"

	"github.com/kevlarcon/warp/pkg/jq"
)

// TestTryJqStagePushdown_StringInterp pins partial pushdown for jq pipelines
// of the shape `select(...) | "\(.a) \(.b)"`. warp's parser emits a nested
// StringInterp as a single Part for every interpolation after the first
// (see parseStringInterp in warp/pkg/jq/parse.go), so the round-tripper has
// to flatten and re-render as a single `"..."` literal — otherwise
// tryJqStagePushdown can't express the residual and bails on the whole
// pushdown, leaving the select predicate unpushed. That was the user-visible
// bug: `explain` showed ShardSQL without the select.
func TestTryJqStagePushdown_StringInterp(t *testing.T) {
	tests := []struct {
		name          string
		expression    string
		wantPredicate string
		wantCompute   string
	}{
		{
			name:          "two interpolations, literal in between",
			expression:    `select(.type == "disk") | "\(.host) \(.used_gb)"`,
			wantPredicate: `json_extract_string(payload, '$.type') = 'disk'`,
			wantCompute:   `jq '"\(.host) \(.used_gb)"'`,
		},
		{
			name:          "leading literal then interpolation",
			expression:    `select(.type == "disk") | "host: \(.host)"`,
			wantPredicate: `json_extract_string(payload, '$.type') = 'disk'`,
			wantCompute:   `jq '"host: \(.host)"'`,
		},
		{
			name:          "single interpolation, no literals",
			expression:    `select(.type == "disk") | "\(.host)"`,
			wantPredicate: `json_extract_string(payload, '$.type') = 'disk'`,
			wantCompute:   `jq '"\(.host)"'`,
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
			got := tryJqStagePushdown(tt.expression, nil)
			if !got.pushed {
				t.Fatalf("expected pushed=true (predicate %q should extract), got pushdownResult{}", tt.wantPredicate)
			}
			if got.predicate != tt.wantPredicate {
				t.Errorf("predicate\n got: %s\nwant: %s", got.predicate, tt.wantPredicate)
			}
			if got.compute != tt.wantCompute {
				t.Errorf("compute\n got: %s\nwant: %s", got.compute, tt.wantCompute)
			}
		})
	}
}
