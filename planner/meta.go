package planner

import (
	"slices"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// MetaRead is which of a row's meta-variables a stage reads (an awk's HOST, a
// jq's $host), as the shard columns that back them, or all of them when the
// stage can't be told (awk -f, a jq program that doesn't parse). The shard
// projects, and compute builds and binds, only those: a row has about twenty
// and a stage reads one or two. The zero value reads none.
type MetaRead struct {
	All  bool
	cols []string // distinct, in metaVarTable order; ts, event_id and line_no included
	proj []string // cols that are shard columns to project, in MetaVarColumns order
}

// metaReadOf is the MetaRead of a stage that reads these columns.
func metaReadOf(cols []string) MetaRead {
	var m MetaRead
	for _, v := range metaVarTable {
		if slices.Contains(cols, v.column) && !slices.Contains(m.cols, v.column) {
			m.cols = append(m.cols, v.column)
		}
	}
	for _, c := range MetaVarColumns {
		if slices.Contains(m.cols, c) {
			m.proj = append(m.proj, c)
		}
	}
	return m
}

// allMeta is a stage that may read any of them.
var allMeta = MetaRead{All: true}

// merge is what either of two stages reads.
func (m MetaRead) merge(o MetaRead) MetaRead {
	if m.All || o.All {
		return allMeta
	}
	return metaReadOf(append(slices.Clone(m.cols), o.cols...))
}

// Reads reports whether the stage reads any meta-variable.
func (m MetaRead) Reads() bool { return m.All || len(m.cols) > 0 }

// Wants reports whether the stage reads the column: one of MetaVarColumns, or
// ts, event_id or line_no, which every row carries as its own field.
func (m MetaRead) Wants(col string) bool { return m.All || slices.Contains(m.cols, col) }

// Columns is the shard columns to project for it, in the order of
// MetaVarColumns. ts, event_id and line_no are not among them: every query
// selects those.
func (m MetaRead) Columns() []string {
	if m.All {
		return MetaVarColumns
	}
	return m.proj
}

// awkMetaRead is the meta-variables an awk program names.
func awkMetaRead(prog *awk.Program) MetaRead {
	var cols []string
	astContains(prog, func(n any) bool {
		if id, ok := n.(*awk.IdentExpr); ok {
			if col := AwkMetaVars[id.Name]; col != "" {
				cols = append(cols, col)
			}
		}
		return false
	})
	return metaReadOf(cols)
}

// StageMetaRead is the meta-variables a stage, as written, reads. Compute
// carries them to the stage, and builds them per row, only then.
func StageMetaRead(stage string) MetaRead {
	sc, ok := parseOneStage(stage)
	if !ok {
		return MetaRead{}
	}
	return lowerStage(parseStage(sc), 1, false).meta
}
