package compute

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// aggregator finishes a shard-side aggregate (planner.QueryPlan.Aggregate).
// Each shard sends a distinct line per row with its count, in merge-key
// order, so after the k-way merge equal lines from different shards are
// adjacent: the aggregator sums their counts and passes on the text the
// aggregated stages print, as warp prints it (uniq -c's "%7d %s", wc -l's
// bare total). The differential harness pins the two together.
type aggregator struct {
	fn   string
	sink func(*pb.ResultRow) bool
	line string
	n    int64
	// groups counts the distinct lines emit has taken (distinctTotal).
	groups int64
	seen   bool
	err    error
	// collect, when set, takes each merged group instead of the sink
	// (orderedAggregator).
	collect *[]countedLine
}

type countedLine struct {
	line string
	n    int64
}

// orderedAggregator is a count_distinct aggregator whose groups print
// highest count first, cut to the limit (0 keeps all): `sort -rn | head -n N`
// after uniq -c. It holds one entry per distinct line until flush. Equal
// counts print the line in descending byte order, as sort -rn leaves them
// (its -r reverses the last-resort comparison too); the differential test
// against the stages pins that.
type orderedAggregator struct {
	*aggregator
	out    func(*pb.ResultRow) bool
	limit  int
	groups []countedLine
}

func newOrderedAggregator(a *aggregator, limit int, sink func(*pb.ResultRow) bool) *orderedAggregator {
	o := &orderedAggregator{aggregator: a, out: sink, limit: limit}
	a.collect = &o.groups
	return o
}

func (o *orderedAggregator) flush() {
	o.aggregator.flush()
	if o.err != nil {
		return
	}
	slices.SortFunc(o.groups, func(x, y countedLine) int {
		if c := cmp.Compare(y.n, x.n); c != 0 {
			return c
		}
		return strings.Compare(y.line, x.line)
	})
	if o.limit > 0 && len(o.groups) > o.limit {
		o.groups = o.groups[:o.limit]
	}
	for _, g := range o.groups {
		if !o.out(&pb.ResultRow{Raw: fmt.Sprintf("%7d %s", g.n, g.line)}) {
			return
		}
	}
}

type rowAggregator interface {
	push(*pb.ResultRow) bool
	flush()
	failure() error
}

// The aggregate names a coordinator that predates GroupSpec sends in
// ExecuteQueryRequest.Aggregate. They are also the aggregator's own names
// for these shapes. Delete the wire use with that coordinator's last
// release (docs/plans/internal-wire-versioning.md).
const (
	// legacyLines is `wc -l`: the total.
	legacyLines = "lines"
	// legacyDistinct is `sort -u` or `sort | uniq`: each line once.
	legacyDistinct = "distinct"
	// legacyCountDistinct is `sort | uniq -c`: each line with its count,
	// as uniq -c prints it.
	legacyCountDistinct = "count_distinct"
)

// distinctTotal is `sort -u | wc -l`: the number of distinct lines. It has no
// legacy aggregate string; only a GroupSpec asks for it.
const distinctTotal = "distinct_total"

// GroupToProto is the GroupSpec a coordinator sends for a plan's grouping,
// nil for none.
func GroupToProto(g *planner.GroupSpec) *pb.GroupSpec {
	if g == nil {
		return nil
	}
	switch g.Render {
	case planner.RenderTotal:
		return &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_TOTAL}
	case planner.RenderLine:
		return &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_LINE}
	case planner.RenderCountLine:
		return &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE}
	}
	panic("compute: no GroupSpec for render " + string(g.Render))
}

// groupAggregate is the aggregate a GroupSpec asks for, named as the
// legacy aggregate string for the same shape.
func groupAggregate(g *pb.GroupSpec) (string, error) {
	switch g.Render {
	case pb.GroupRender_GROUP_RENDER_TOTAL:
		return legacyLines, nil
	case pb.GroupRender_GROUP_RENDER_LINE:
		return legacyDistinct, nil
	case pb.GroupRender_GROUP_RENDER_COUNT_LINE:
		return legacyCountDistinct, nil
	case pb.GroupRender_GROUP_RENDER_DISTINCT_TOTAL:
		return distinctTotal, nil
	}
	return "", fmt.Errorf("compute: unknown group render %s", g.Render)
}

// groupOrder is whether a GroupSpec orders its groups by count and how many
// it keeps. An order or limit no shape uses is an error: ignoring it would
// print the wrong text without a sign of it.
func groupOrder(g *pb.GroupSpec) (byCount bool, limit int, err error) {
	if g.Limit < 0 {
		return false, 0, fmt.Errorf("compute: negative group limit %d", g.Limit)
	}
	switch g.Order {
	case pb.GroupOrder_GROUP_ORDER_UNSPECIFIED:
		if g.Limit != 0 {
			return false, 0, errors.New("compute: a group limit needs an order")
		}
		return false, 0, nil
	case pb.GroupOrder_GROUP_ORDER_COUNT_DESC:
		if g.Render != pb.GroupRender_GROUP_RENDER_COUNT_LINE {
			return false, 0, fmt.Errorf("compute: a group ordered by count needs %s, not %s", pb.GroupRender_GROUP_RENDER_COUNT_LINE, g.Render)
		}
		return true, int(g.Limit), nil
	}
	return false, 0, fmt.Errorf("compute: unknown group order %s", g.Order)
}

func newAggregator(fn string, sink func(*pb.ResultRow) bool) (rowAggregator, error) {
	switch fn {
	case planner.AggregateSpans:
		return &spanAggregator{sink: sink, groups: map[spanKey]*spanGroup{}}, nil
	case planner.AggregateTraceIDs:
		return &traceIDsAggregator{sink: sink, last: map[string]int64{}}, nil
	case planner.AggregateTraceList:
		return &traceListAggregator{sink: sink, groups: map[string]*traceGroup{}}, nil
	case planner.AggregateSpanRows, planner.AggregateSpanSamples:
		return &spanRowAggregator{fn: fn, sink: sink}, nil
	case legacyLines, legacyDistinct, legacyCountDistinct, distinctTotal:
		return &aggregator{fn: fn, sink: sink}, nil
	}
	return nil, fmt.Errorf("compute: unknown aggregate %q", fn)
}

// push takes one merged shard row. It returns false to stop the merge.
func (a *aggregator) push(row *pb.ResultRow) bool {
	n, err := strconv.ParseInt(row.MetaVars[planner.CountColumn], 10, 64)
	if err != nil {
		a.err = fmt.Errorf("compute: aggregate row without a %s: %w", planner.CountColumn, err)
		return false
	}
	if a.fn != legacyLines && a.seen && row.Raw != a.line {
		if !a.emit() {
			return false
		}
		a.n = 0
	}
	a.line, a.seen = row.Raw, true
	a.n += n
	return true
}

func (a *aggregator) failure() error { return a.err }

// flush emits the last line. wc -l prints its total even with no rows, and
// so does the distinct count.
func (a *aggregator) flush() {
	if a.err != nil {
		return
	}
	if a.seen || a.fn == legacyLines {
		a.emit()
	}
	if a.fn == distinctTotal {
		a.sink(&pb.ResultRow{Raw: strconv.FormatInt(a.groups, 10)})
	}
}

func (a *aggregator) emit() bool {
	if a.collect != nil {
		*a.collect = append(*a.collect, countedLine{a.line, a.n})
		return true
	}
	var text string
	switch a.fn {
	case distinctTotal:
		a.groups++
		return true
	case legacyLines:
		text = strconv.FormatInt(a.n, 10)
	case legacyDistinct:
		text = a.line
	case legacyCountDistinct:
		text = fmt.Sprintf("%7d %s", a.n, a.line)
	}
	return a.sink(&pb.ResultRow{Raw: text})
}
