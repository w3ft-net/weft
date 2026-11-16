package planner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// rowStage.nodes is the one rule by which awk and jq fold a filter and a
// projection into the IR.
func TestRowStageNodes(t *testing.T) {
	why := because(reasonAwkAction)
	tests := []struct {
		name string
		rs   rowStage
		want string
	}{
		{"filter", rowStage{pred: colRef("p"), predExact: true}, "filter(p,exact)"},
		{"filter and project", rowStage{pred: colRef("p"), predExact: true, proj: colRef("e")}, "filter(p,exact) project(e)"},
		{"project", rowStage{proj: colRef("e")}, "project(e)"},
		{"prefilter runs the stage", rowStage{pred: colRef("p"), opaque: true, why: why}, "filter(p,prefilter) opaque"},
		{"nothing lowers", rowStage{opaque: true, why: why}, "opaque"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, n := range tt.rs.nodes(1) {
				switch n := n.(type) {
				case *filterNode:
					kind := "prefilter"
					if n.exact {
						kind = "exact"
					}
					got = append(got, fmt.Sprintf("filter(%s,%s)", render(n.pred, dialect.DuckDB), kind))
				case *projectNode:
					got = append(got, fmt.Sprintf("project(%s)", render(n.expr, dialect.DuckDB)))
				case *opaqueNode:
					got = append(got, "opaque")
				}
			}
			if s := strings.Join(got, " "); s != tt.want {
				t.Errorf("nodes = %q, want %q", s, tt.want)
			}
		})
	}
}

// awk and jq spell the same meta-variables.
func TestMetaVarsShareOneTable(t *testing.T) {
	if len(AwkMetaVars) != len(JQMetaVars) {
		t.Fatalf("awk has %d meta-variables, jq %d", len(AwkMetaVars), len(JQMetaVars))
	}
	for name, col := range JQMetaVars {
		if AwkMetaVars[strings.ToUpper(name)] != col {
			t.Errorf("%s: jq reads %s, awk reads %q", name, col, AwkMetaVars[strings.ToUpper(name)])
		}
	}
}
