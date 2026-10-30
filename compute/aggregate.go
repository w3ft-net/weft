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
	// groups counts the distinct lines emit has taken (aggDistinctTotal).
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

// The aggregators a GroupSpec resolves to, named by what they print.
const (
	// aggLines is `wc -l`: the total.
	aggLines = "lines"
	// aggDistinct is `sort -u` or `sort | uniq`: each line once.
	aggDistinct = "distinct"
	// aggCountDistinct is `sort | uniq -c`: each line with its count, as
	// uniq -c prints it.
	aggCountDistinct = "count_distinct"
	// aggDistinctTotal is `sort -u | wc -l`: the number of distinct lines.
	aggDistinctTotal = "distinct_total"
	// aggKeyCount is an awk tally, `key count` per distinct line.
	aggKeyCount = "key_count"
	// aggBytes is `wc -c`: the shards' byte totals summed.
	aggBytes = "bytes"
	// aggNumericMax and aggNumericMin are the line sort -n puts last or first.
	aggNumericMax = "numeric_max"
	aggNumericMin = "numeric_min"
)

// isGroupAggregate is whether name is one of the group aggregators, which
// only a GroupSpec asks for: a request's aggregate string names the trace and
// span aggregates.
func isGroupAggregate(name string) bool {
	switch name {
	case aggLines, aggDistinct, aggCountDistinct, aggDistinctTotal, aggKeyCount, aggBytes, aggNumericMax, aggNumericMin:
		return true
	}
	return false
}

// GroupToProto is the GroupSpec a coordinator sends for a plan's grouping,
// nil for none.
func GroupToProto(g *planner.GroupSpec) *pb.GroupSpec {
	if g == nil {
		return nil
	}
	out := &pb.GroupSpec{Limit: int64(g.Limit)}
	switch g.Render {
	case planner.RenderTotal:
		out.Render = pb.GroupRender_GROUP_RENDER_TOTAL
	case planner.RenderLine:
		out.Render = pb.GroupRender_GROUP_RENDER_LINE
	case planner.RenderCountLine:
		out.Render = pb.GroupRender_GROUP_RENDER_COUNT_LINE
	case planner.RenderDistinctTotal:
		out.Render = pb.GroupRender_GROUP_RENDER_DISTINCT_TOTAL
	case planner.RenderKeyCount:
		out.Render = pb.GroupRender_GROUP_RENDER_KEY_COUNT
	case planner.RenderBytes:
		out.Render = pb.GroupRender_GROUP_RENDER_BYTES
	case planner.RenderNumericMax:
		out.Render = pb.GroupRender_GROUP_RENDER_NUMERIC_MAX
	case planner.RenderNumericMin:
		out.Render = pb.GroupRender_GROUP_RENDER_NUMERIC_MIN
	default:
		panic("compute: no GroupSpec for render " + string(g.Render))
	}
	if g.ByCount {
		out.Order = pb.GroupOrder_GROUP_ORDER_COUNT_DESC
	}
	return out
}

// groupAggregate is the aggregator a GroupSpec asks for.
func groupAggregate(g *pb.GroupSpec) (string, error) {
	switch g.Render {
	case pb.GroupRender_GROUP_RENDER_TOTAL:
		return aggLines, nil
	case pb.GroupRender_GROUP_RENDER_LINE:
		return aggDistinct, nil
	case pb.GroupRender_GROUP_RENDER_COUNT_LINE:
		return aggCountDistinct, nil
	case pb.GroupRender_GROUP_RENDER_DISTINCT_TOTAL:
		return aggDistinctTotal, nil
	case pb.GroupRender_GROUP_RENDER_KEY_COUNT:
		return aggKeyCount, nil
	case pb.GroupRender_GROUP_RENDER_BYTES:
		return aggBytes, nil
	case pb.GroupRender_GROUP_RENDER_NUMERIC_MAX:
		return aggNumericMax, nil
	case pb.GroupRender_GROUP_RENDER_NUMERIC_MIN:
		return aggNumericMin, nil
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
	case aggLines, aggDistinct, aggCountDistinct, aggDistinctTotal, aggKeyCount:
		return &aggregator{fn: fn, sink: sink}, nil
	case aggBytes:
		return &bytesAggregator{sink: sink}, nil
	case aggNumericMax, aggNumericMin:
		return &extremeAggregator{max: fn == aggNumericMax, sink: sink}, nil
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
	if a.fn != aggLines && a.seen && row.Raw != a.line {
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
	if a.seen || a.fn == aggLines {
		a.emit()
	}
	if a.fn == aggDistinctTotal {
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
	case aggDistinctTotal:
		a.groups++
		return true
	case aggLines:
		text = strconv.FormatInt(a.n, 10)
	case aggDistinct:
		text = a.line
	case aggCountDistinct:
		text = fmt.Sprintf("%7d %s", a.n, a.line)
	case aggKeyCount:
		text = fmt.Sprintf("%s %d", a.line, a.n)
	}
	return a.sink(&pb.ResultRow{Raw: text})
}

// bytesAggregator is `wc -c`: each shard sends one row with its byte total
// (planner.BytesColumn), and the total prints even when no shard had a line.
type bytesAggregator struct {
	sink  func(*pb.ResultRow) bool
	total int64
	err   error
}

func (b *bytesAggregator) push(row *pb.ResultRow) bool {
	n, err := strconv.ParseInt(row.MetaVars[planner.BytesColumn], 10, 64)
	if err != nil {
		b.err = fmt.Errorf("compute: aggregate row without a %s: %w", planner.BytesColumn, err)
		return false
	}
	b.total += n
	return true
}

func (b *bytesAggregator) flush() {
	if b.err == nil {
		b.sink(&pb.ResultRow{Raw: strconv.FormatInt(b.total, 10)})
	}
}

func (b *bytesAggregator) failure() error { return b.err }

// extremeAggregator is the line sort -n puts last (max) or first: each shard
// sends its best line with the number sort compares it by
// (planner.SortKeyColumn), and the best of those is the answer. Equal numbers
// go to the greater line (the lesser for min), as sort's last-resort compare
// of the lines does. With no line at all it prints nothing.
type extremeAggregator struct {
	max  bool
	sink func(*pb.ResultRow) bool
	err  error
	seen bool
	key  float64
	line string
}

func (e *extremeAggregator) push(row *pb.ResultRow) bool {
	key, err := strconv.ParseFloat(row.MetaVars[planner.SortKeyColumn], 64)
	if err != nil {
		e.err = fmt.Errorf("compute: aggregate row without a %s: %w", planner.SortKeyColumn, err)
		return false
	}
	c := cmp.Compare(key, e.key)
	if c == 0 {
		c = strings.Compare(row.Raw, e.line)
	}
	if !e.seen || (e.max && c > 0) || (!e.max && c < 0) {
		e.seen, e.key, e.line = true, key, row.Raw
	}
	return true
}

func (e *extremeAggregator) flush() {
	if e.err == nil && e.seen {
		e.sink(&pb.ResultRow{Raw: e.line})
	}
}

func (e *extremeAggregator) failure() error { return e.err }
