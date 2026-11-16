package planner

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// TestGroupOrder (and the distinct count): `sort -rn | head -n N` right after sort | uniq -c is the
// group's order and limit, and no other shape of those stages is.
func TestGroupOrder(t *testing.T) {
	for _, tt := range []struct {
		pipeline string
		want     GroupBySpec
		compute  int // stages left for compute
	}{
		{`cat vimgym.log | sort | uniq -c`, GroupBySpec{Render: RenderCountLine}, 0},
		{`cat vimgym.log | sort | uniq -c | sort -rn`, GroupBySpec{Render: RenderCountLine, ByCount: true}, 0},
		{`cat vimgym.log | sort | uniq -c | sort -rn | head -n 5`, GroupBySpec{Render: RenderCountLine, ByCount: true, Limit: 5}, 0},
		{`cat vimgym.log | sort | uniq -c | sort -nr | head -3`, GroupBySpec{Render: RenderCountLine, ByCount: true, Limit: 3}, 0},
		{`cat vimgym.log | sort -r | uniq -c | sort -rn | head -n 5`, GroupBySpec{Render: RenderCountLine, ByCount: true, Limit: 5}, 0},
		// head -n 0 prints nothing, but a limit of 0 means every group.
		{`cat vimgym.log | sort | uniq -c | sort -rn | head -n 0`, GroupBySpec{Render: RenderCountLine, ByCount: true}, 1},
		// A stage after the head still runs in compute.
		{`cat vimgym.log | sort | uniq -c | sort -rn | head -n 5 | grep a`, GroupBySpec{Render: RenderCountLine, ByCount: true, Limit: 5}, 1},
		// Not the highest-count-first order: they stay stages.
		{`cat vimgym.log | sort | uniq -c | sort -n | head -n 5`, GroupBySpec{Render: RenderCountLine}, 2},
		{`cat vimgym.log | sort | uniq -c | sort -rn -k2 | head -n 5`, GroupBySpec{Render: RenderCountLine}, 2},
		{`cat vimgym.log | sort | uniq -c | sort -run | head -n 5`, GroupBySpec{Render: RenderCountLine}, 2},
		{`cat vimgym.log | sort | uniq -c | sort -rn | tail -n 5`, GroupBySpec{Render: RenderCountLine, ByCount: true}, 1},
		// uniq's own count of lines: a count of the distinct ones.
		{`cat vimgym.log | sort -u | wc -l`, GroupBySpec{Render: RenderDistinctTotal}, 0},
		{`cat vimgym.log | sort | uniq | wc -l`, GroupBySpec{Render: RenderDistinctTotal}, 0},
		{`cat vimgym.log | sort -r | uniq | wc -l`, GroupBySpec{Render: RenderDistinctTotal}, 0},
		{`cat vimgym.log | sort -u | wc -l | awk '{ print $1 + 1 }'`, GroupBySpec{Render: RenderDistinctTotal}, 1},
		// Not a count of distinct lines: uniq -c's lines are, but sort -n compares keys.
		{`cat vimgym.log | sort -n | uniq | wc -l`, GroupBySpec{}, 3},
		{`cat vimgym.log | sort | wc -l`, GroupBySpec{}, 2},
		// wc -c: the bytes of the lines, newlines included.
		{`cat vimgym.log | wc -c`, GroupBySpec{Render: RenderBytes}, 0},
		{`cat vimgym.log | grep a | wc -c`, GroupBySpec{Render: RenderBytes}, 0},
		{`cat vimgym.log | wc -c | awk '{ print $1 + 1 }'`, GroupBySpec{Render: RenderBytes}, 1},
		// Other wc counts and flag mixes stay stages.
		{`cat vimgym.log | wc -m`, GroupBySpec{}, 1},
		{`cat vimgym.log | wc -lc`, GroupBySpec{}, 1},
		{`cat vimgym.log | wc -c -l`, GroupBySpec{}, 1},
		{`tail -n 5 vimgym.log | wc -c`, GroupBySpec{}, 1},
		// sort -n's last or first line: the greatest or least number, ties by
		// the line's bytes, in either spelling.
		{`cat vimgym.log | sort -n | tail -n 1`, GroupBySpec{Render: RenderNumericMax}, 0},
		{`cat vimgym.log | sort -n | tail -1`, GroupBySpec{Render: RenderNumericMax}, 0},
		{`cat vimgym.log | sort -rn | head -n 1`, GroupBySpec{Render: RenderNumericMax}, 0},
		{`cat vimgym.log | sort -nr | head -1`, GroupBySpec{Render: RenderNumericMax}, 0},
		{`cat vimgym.log | sort -n | head -n 1`, GroupBySpec{Render: RenderNumericMin}, 0},
		{`cat vimgym.log | sort -rn | tail -n 1`, GroupBySpec{Render: RenderNumericMin}, 0},
		{`cat vimgym.log | grep a | awk '{ print $2 }' | sort -n | tail -n 1`, GroupBySpec{Render: RenderNumericMax}, 0},
		{`cat vimgym.log | sort -n | tail -n 1 | awk '{ print $1 + 1 }'`, GroupBySpec{Render: RenderNumericMax}, 1},
		// Not one line off the end that sort -n compares: they stay stages.
		{`cat vimgym.log | sort -n | tail -n 2`, GroupBySpec{}, 2},
		{`cat vimgym.log | sort -n | head -n 0`, GroupBySpec{}, 2},
		{`cat vimgym.log | sort -n | tail -n +2`, GroupBySpec{}, 2},
		{`cat vimgym.log | sort -n`, GroupBySpec{}, 1},
		{`cat vimgym.log | sort -n -k2 | tail -n 1`, GroupBySpec{}, 2},
		{`cat vimgym.log | sort -n | grep a | tail -n 1`, GroupBySpec{}, 3},
		// A pushed line count is applied after the shards.
		{`tail -n 5 vimgym.log | sort -n | tail -n 1`, GroupBySpec{}, 2},
		// awk's count per key.
		{`cat vimgym.log | awk '{ c[$2]++ } END { for (k in c) print k, c[k] }'`, GroupBySpec{Render: RenderKeyCount}, 0},
		{`cat vimgym.log | grep a | awk '{ n[$1 "|" $2]++ } END { for (x in n) print x, n[x] }'`, GroupBySpec{Render: RenderKeyCount}, 0},
		{`cat vimgym.log | awk '{ ++c[$2] } END { for (k in c) print k, c[k] }'`, GroupBySpec{Render: RenderKeyCount}, 0},
		{`cat vimgym.log | awk '{ c[$2]++ } END { for (k in c) print k, c[k] }' | sort -rn`, GroupBySpec{Render: RenderKeyCount}, 1},
		// Not the whole tally shape: a pattern, a sum, another print, a second
		// array, an order of the output, a key SQL can't compute.
		{`cat vimgym.log | awk '/a/ { c[$2]++ } END { for (k in c) print k, c[k] }'`, GroupBySpec{}, 1},
		{`cat vimgym.log | awk '{ c[$2] += $3 } END { for (k in c) print k, c[k] }'`, GroupBySpec{}, 1},
		{`cat vimgym.log | awk '{ c[$2]++ } END { for (k in c) print c[k], k }'`, GroupBySpec{}, 1},
		{`cat vimgym.log | awk '{ c[$2]++; d[$3]++ } END { for (k in c) print k, c[k] }'`, GroupBySpec{}, 1},
		{`cat vimgym.log | awk '{ c[$2]++ } END { for (k in c) print k, c[k] > "/dev/stderr" }'`, GroupBySpec{}, 1},
		{`cat vimgym.log | awk '{ c[toupper($2)]++ } END { for (k in c) print k, c[k] }'`, GroupBySpec{}, 1},
		// The key would read the text a projection ahead of it made.
		{`cat vimgym.log | cut -d ' ' -f 2- | awk '{ c[$1]++ } END { for (k in c) print k, c[k] }'`, GroupBySpec{}, 1},
		// A pushed line count is applied after the shards.
		{`tail -n 5 vimgym.log | awk '{ c[$2]++ } END { for (k in c) print k, c[k] }'`, GroupBySpec{}, 1},
		// An order has no meaning over a line or a total.
		{`cat vimgym.log | sort -u | sort -rn | head -n 5`, GroupBySpec{Render: RenderLine}, 2},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			_, c, err := compileBare(dialect.DuckDB, tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.want == (GroupBySpec{}) && c.group != nil:
				t.Errorf("group = %+v, want none", c.group)
			case tt.want != (GroupBySpec{}) && (c.group == nil || *c.group != tt.want):
				t.Errorf("group = %+v, want %+v", c.group, tt.want)
			}
			if len(c.compute) != tt.compute {
				t.Errorf("%d stages left for compute, want %d", len(c.compute), tt.compute)
			}
		})
	}
}

// TestNumericEndNeedsPlainSort: sort -u keeps the first of lines with equal
// numbers, which depends on the order the rows arrive in, so sort -nu stays
// stages. (Written as suffixes so the differential corpus, which runs every
// pipeline literal in these files, doesn't compare the arrival-order case.)
func TestNumericEndNeedsPlainSort(t *testing.T) {
	for _, tail := range []string{"sort -nu | tail -n 1", "sort -nru | head -n 1"} {
		_, c, err := compileBare(dialect.DuckDB, "cat vimgym.log | "+tail, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if c.group != nil || len(c.compute) != 2 {
			t.Errorf("%s: group %+v with %d compute stages, want none and 2", tail, c.group, len(c.compute))
		}
	}
}
