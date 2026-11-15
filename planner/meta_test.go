package planner

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"
)

var time0 time.Time

// hasColumn reports whether the query's SELECT list has the column.
func hasColumn(sql, col string) bool {
	list, _, _ := strings.Cut(strings.TrimPrefix(sql, "SELECT "), " FROM ")
	return slices.Contains(strings.Split(list, ", "), col)
}

// A stage reads some of a row's meta-variables, or can't be told which; the
// shard projects and compute binds only those.
func TestStageMetaRead(t *testing.T) {
	for _, c := range []struct {
		stage   string
		all     bool
		columns []string // the projected shard columns, in table order
		wants   []string // also ts, event_id, line_no
	}{
		{stage: `grep x`},
		{stage: `awk '$1 == "x"'`},
		{stage: `awk 'LEVEL == "ERROR"'`, columns: []string{"level"}},
		{stage: `awk '{ print HOST, LEVEL, NAMESPACE }'`, columns: []string{"namespace", "level", "instance"}},
		{stage: `awk '{ print INSTANCE, HOST }'`, columns: []string{"instance"}},
		{stage: `awk '{ print TS, LINE_NO, $1 }'`, wants: []string{"ts", "line_no"}},
		{stage: `awk -f prog.awk`, all: true},
		{stage: `jq -c '. + {l: $level}'`, columns: []string{"level"}},
		{stage: `jq -c '[$host, $pod, .q]'`, columns: []string{"instance", "pod"}},
		{stage: `jq -c '[$ts, $event_id]'`, wants: []string{"ts", "event_id"}},
		{stage: `jq -c '. as $level | $level'`, columns: []string{"level"}},
		{stage: `jq -c '.q'`},
		{stage: `jq -c`, all: true}, // no program to read
	} {
		m := StageMetaRead(c.stage)
		if m.All != c.all {
			t.Errorf("%s: All = %v, want %v", c.stage, m.All, c.all)
		}
		if c.all {
			if got := m.Columns(); !slices.Equal(got, MetaVarColumns) {
				t.Errorf("%s: columns %v, want every one", c.stage, got)
			}
			continue
		}
		if got := m.Columns(); !slices.Equal(got, c.columns) {
			t.Errorf("%s: columns %v, want %v", c.stage, got, c.columns)
		}
		for _, w := range append(slices.Clone(c.columns), c.wants...) {
			if !m.Wants(w) {
				t.Errorf("%s: doesn't want %s", c.stage, w)
			}
		}
		if reads := len(c.columns)+len(c.wants) > 0; m.Reads() != reads {
			t.Errorf("%s: Reads = %v, want %v", c.stage, m.Reads(), reads)
		}
	}
}

// The shard projects the union of what the stages that run in compute read.
func TestPlan_ProjectsOnlyTheMetaColumnsStagesRead(t *testing.T) {
	for _, c := range []struct {
		pipeline string
		has      []string
		lacks    []string
	}{
		{`cat vimgym.log | awk '{ if (LEVEL == "ERROR") e++ } END { print e }'`, []string{"level"}, []string{"deployment", "pod", "instance", "trace_id"}},
		{`cat vimgym.log | awk '{ if (LEVEL == "ERROR") e++ } END { print e }' | awk '{ print HOST }'`, []string{"level"}, []string{"instance"}},
		{`cat vimgym.log | jq -c '. + {h: $host}'`, []string{"instance"}, []string{"level", "deployment"}},
		{`cat vimgym.log | awk -f prog.awk`, MetaVarColumns, nil},
		{`cat vimgym.log | awk '{ n += $2 } END { print n }'`, nil, MetaVarColumns},
	} {
		plan, err := Plan(c.pipeline, shapetest.Tenant(1), "", time0, time0)
		if err != nil {
			t.Fatalf("%s: %v", c.pipeline, err)
		}
		for _, col := range c.has {
			if !hasColumn(plan.DirectShardSQL, col) {
				t.Errorf("%s: SQL lacks %s:\n%s", c.pipeline, col, plan.DirectShardSQL)
			}
		}
		for _, col := range c.lacks {
			if hasColumn(plan.DirectShardSQL, col) {
				t.Errorf("%s: SQL projects %s it doesn't need:\n%s", c.pipeline, col, plan.DirectShardSQL)
			}
		}
	}
}
