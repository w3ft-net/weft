package compute

import (
	"fmt"
	"strconv"

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
	seen bool
	err  error
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
	}
	return "", fmt.Errorf("compute: unknown group render %s", g.Render)
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
	case legacyLines, legacyDistinct, legacyCountDistinct:
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

// flush emits the last line. wc -l prints its total even with no rows.
func (a *aggregator) flush() {
	if a.err == nil && (a.seen || a.fn == legacyLines) {
		a.emit()
	}
}

func (a *aggregator) emit() bool {
	var text string
	switch a.fn {
	case legacyLines:
		text = strconv.FormatInt(a.n, 10)
	case legacyDistinct:
		text = a.line
	case legacyCountDistinct:
		text = fmt.Sprintf("%7d %s", a.n, a.line)
	}
	return a.sink(&pb.ResultRow{Raw: text})
}
