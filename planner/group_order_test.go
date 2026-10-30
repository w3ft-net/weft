package planner

import (
	"testing"
	"time"
)

// TestGroupOrder (and the distinct count): `sort -rn | head -n N` right after sort | uniq -c is the
// group's order and limit, and no other shape of those stages is.
func TestGroupOrder(t *testing.T) {
	for _, tt := range []struct {
		pipeline string
		want     GroupSpec
		compute  int // stages left for compute
	}{
		{`cat vimgym.log | sort | uniq -c`, GroupSpec{Render: RenderCountLine}, 0},
		{`cat vimgym.log | sort | uniq -c | sort -rn`, GroupSpec{Render: RenderCountLine, ByCount: true}, 0},
		{`cat vimgym.log | sort | uniq -c | sort -rn | head -n 5`, GroupSpec{Render: RenderCountLine, ByCount: true, Limit: 5}, 0},
		{`cat vimgym.log | sort | uniq -c | sort -nr | head -3`, GroupSpec{Render: RenderCountLine, ByCount: true, Limit: 3}, 0},
		{`cat vimgym.log | sort -r | uniq -c | sort -rn | head -n 5`, GroupSpec{Render: RenderCountLine, ByCount: true, Limit: 5}, 0},
		// head -n 0 prints nothing, but a limit of 0 means every group.
		{`cat vimgym.log | sort | uniq -c | sort -rn | head -n 0`, GroupSpec{Render: RenderCountLine, ByCount: true}, 1},
		// A stage after the head still runs in compute.
		{`cat vimgym.log | sort | uniq -c | sort -rn | head -n 5 | grep a`, GroupSpec{Render: RenderCountLine, ByCount: true, Limit: 5}, 1},
		// Not the highest-count-first order: they stay stages.
		{`cat vimgym.log | sort | uniq -c | sort -n | head -n 5`, GroupSpec{Render: RenderCountLine}, 2},
		{`cat vimgym.log | sort | uniq -c | sort -rn -k2 | head -n 5`, GroupSpec{Render: RenderCountLine}, 2},
		{`cat vimgym.log | sort | uniq -c | sort -run | head -n 5`, GroupSpec{Render: RenderCountLine}, 2},
		{`cat vimgym.log | sort | uniq -c | sort -rn | tail -n 5`, GroupSpec{Render: RenderCountLine, ByCount: true}, 1},
		// uniq's own count of lines: a count of the distinct ones.
		{`cat vimgym.log | sort -u | wc -l`, GroupSpec{Render: RenderDistinctTotal}, 0},
		{`cat vimgym.log | sort | uniq | wc -l`, GroupSpec{Render: RenderDistinctTotal}, 0},
		{`cat vimgym.log | sort -r | uniq | wc -l`, GroupSpec{Render: RenderDistinctTotal}, 0},
		{`cat vimgym.log | sort -u | wc -l | awk '{ print $1 + 1 }'`, GroupSpec{Render: RenderDistinctTotal}, 1},
		// Not a count of distinct lines: uniq -c's lines are, but sort -n compares keys.
		{`cat vimgym.log | sort -n | uniq | wc -l`, GroupSpec{}, 3},
		{`cat vimgym.log | sort | wc -l`, GroupSpec{}, 2},
		// An order has no meaning over a line or a total.
		{`cat vimgym.log | sort -u | sort -rn | head -n 5`, GroupSpec{Render: RenderLine}, 2},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			_, c, err := compile(tt.pipeline, 0, "", time.Time{}, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.want == (GroupSpec{}) && c.group != nil:
				t.Errorf("group = %+v, want none", c.group)
			case tt.want != (GroupSpec{}) && (c.group == nil || *c.group != tt.want):
				t.Errorf("group = %+v, want %+v", c.group, tt.want)
			}
			if len(c.compute) != tt.compute {
				t.Errorf("%d stages left for compute, want %d", len(c.compute), tt.compute)
			}
		})
	}
}
