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
	case planner.AggregateLines, planner.AggregateDistinct, planner.AggregateCountDistinct:
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
	if a.fn != planner.AggregateLines && a.seen && row.Raw != a.line {
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
	if a.err == nil && (a.seen || a.fn == planner.AggregateLines) {
		a.emit()
	}
}

func (a *aggregator) emit() bool {
	var text string
	switch a.fn {
	case planner.AggregateLines:
		text = strconv.FormatInt(a.n, 10)
	case planner.AggregateDistinct:
		text = a.line
	case planner.AggregateCountDistinct:
		text = fmt.Sprintf("%7d %s", a.n, a.line)
	}
	return a.sink(&pb.ResultRow{Raw: text})
}
