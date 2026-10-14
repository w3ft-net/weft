package planner

import (
	"fmt"
	"slices"
)

// compiled is a pipeline's IR after the passes ran: which nodes the
// shard computes, which stages run in compute and how, and the log of
// what each pass did. Plan renders it; Explain shows it.
type compiled struct {
	source sourceInfo
	stages []*loweredStage // stages[i-1] is stage i
	// nodes is every stage's nodes in pipeline order, after the source's
	// own head/tail limit.
	nodes []node
	// shard counts the leading nodes in the shard's WHERE/LIMIT.
	shard int
	// selected is the projection that became the shard's SELECT.
	selected *projectNode
	// rowStages and compute are the stages left to run, as
	// QueryPlan.RowStages and QueryPlan.ComputeStages.
	rowStages, compute []*loweredStage
	// readsPayload: the shard returns the payload, as raw when there are
	// no row stages, for a jq first compute stage.
	readsPayload bool
	readsMeta    bool
	shardOrderBy string
	shardLimit   int
	// aggregate is the shard-side aggregate that replaced the first
	// compute stages, if any; aggregated is those stages' nodes.
	aggregate  string
	aggregated []node
	passes     []passStep
}

// passStep is one line of the pass log: what a pass did or declined.
type passStep struct {
	pass, msg string
}

func (c *compiled) logf(pass, format string, args ...any) {
	c.passes = append(c.passes, passStep{pass: pass, msg: fmt.Sprintf(format, args...)})
}

// stage returns the stage a node came from; nil for the source.
func (c *compiled) stage(n node) *loweredStage {
	if i := n.stageIndex(); i > 0 {
		return c.stages[i-1]
	}
	return nil
}

// label names a node with its span, as Explain shows it: Filter[2: grep].
func (c *compiled) label(n node) string {
	return nodeKind(n) + "[" + c.spanLabel(n) + "]"
}

func (c *compiled) spanLabel(n node) string {
	if s := c.stage(n); s != nil {
		return fmt.Sprintf("%d: %s", s.index, s.cmd)
	}
	return "source: " + c.source.cmd
}

func nodeKind(n node) string {
	switch n.(type) {
	case *filterNode:
		return "Filter"
	case *limitNode:
		return "Limit"
	case *projectNode:
		return "Project"
	case *sortNode:
		return "Sort"
	case *uniqNode:
		return "Uniq"
	case *countNode:
		return "Count"
	}
	return "Opaque"
}

// pushFilters is filter pushdown. Starting from the scan, filters and
// limits go into the shard until the first node that can't:
//
//   - a filter or limit above a limit stays put: a line count ends
//     predicate pushdown (`tail -n 500 | grep x` filters the last 500
//     rows; docs/completed/cross-shard-post-limit-filter.md), and the
//     shard takes one LIMIT;
//   - anything else runs in compute, and so does everything above it.
//
// A prefilter only reaches the shard from right below its own stage's
// opaque node, which its frontend put there.
func (c *compiled) pushFilters() {
	const pass = "filter-pushdown"
	var bound *limitNode
	for i, n := range c.nodes {
		switch n := n.(type) {
		case *filterNode, *limitNode:
			if bound != nil {
				c.logf(pass, "%s held above %s: a line count ends predicate pushdown", c.label(n), c.label(bound))
				c.shard = i
				return
			}
			if l, ok := n.(*limitNode); ok {
				bound = l
				c.logf(pass, "%s into shard SQL", c.label(n))
			} else if n.(*filterNode).exact {
				c.logf(pass, "%s into shard SQL (exact)", c.label(n))
			} else {
				c.logf(pass, "%s into shard SQL (prefilter; its stage still runs)", c.label(n))
			}
			continue
		}
		c.shard = i
		if _, ok := n.(*projectNode); !ok {
			c.logf(pass, "stopped at %s: it runs in compute, and so does every node above it", c.label(n))
		}
		return
	}
	c.shard = len(c.nodes)
}

// hoistFilters is filter hoisting: grep and sed filters above the
// sorts, or the SELECT's projection, where pushdown stopped move below
// them, into the shard. Each keeps or drops a line on its own, so:
//
//   - sort only reorders lines: `sort | grep x` prints what
//     `grep x | sort` does. A sort with -u drops lines equal to one it
//     kept, which the filter treats alike; a keyed sort (-n, -k, a file
//     operand) is left alone, since -u then drops lines that only
//     compare equal;
//   - the filter reads the projection's text, which is the SELECT's
//     raw: `jq -r .a | grep x` filters the projected lines.
//
// The shard stops shipping the lines the filter drops. It filters lines,
// not rows: compute splits each row into lines before the stage reads
// them, so the shard returns lines (Query.Lines) and the hoisted filters
// keep or drop each one.
//
// Declined after a pushed line count, which picks its rows before the
// filter does, in follow mode, where live rows run every stage, for a
// trace source, whose tagged rows a split would drop, and, like
// projection pushdown, when the stage the filters leave right after the
// projection names a meta-variable.
func (c *compiled) hoistFilters() {
	const pass = "filter-hoisting"
	var sorts, filters []node
	end := c.shard // past the last filter that hoists
run:
	for i := c.shard; i < len(c.nodes); i++ {
		switch n := c.nodes[i].(type) {
		case *sortNode:
			if n.keyed {
				break run
			}
			sorts = append(sorts, n)
		case *filterNode:
			if !n.lineWise || (len(sorts) == 0 && c.selected == nil) {
				break run
			}
			filters = append(filters, n)
			end = i + 1
		default:
			break run
		}
	}
	if len(filters) == 0 {
		return
	}
	below := c.nodes[c.shard] // the sort, or else the selected projection
	if _, ok := below.(*sortNode); !ok {
		below = c.selected
	}
	for _, n := range c.nodes[:c.shard] {
		if _, ok := n.(*limitNode); ok {
			c.logf(pass, "skipped %s: a pushed line count picks its rows before the filter", c.label(filters[0]))
			return
		}
	}
	switch {
	case c.source.follow:
		c.logf(pass, "skipped %s: follow runs the stages on live rows", c.label(filters[0]))
		return
	case c.source.cmd == "trace" || c.source.cmd == "trace_spans":
		c.logf(pass, "skipped %s: a trace source", c.label(filters[0]))
		return
	case len(sorts) == 0 && end < len(c.nodes) && c.stage(c.nodes[end]).readsMeta:
		c.logf(pass, "skipped %s: %s names a meta-variable, which it would read from the row if it came first",
			c.label(filters[0]), c.label(c.nodes[end]))
		return
	}
	var moved []node
	for _, n := range c.nodes[c.shard:end] {
		if f, ok := n.(*filterNode); ok {
			f.hoisted = true
			moved = append(moved, f)
			c.logf(pass, "%s moved below %s: the shard filters each line", c.label(f), c.label(below))
		}
	}
	for _, n := range c.nodes[c.shard:end] {
		if _, ok := n.(*sortNode); ok {
			moved = append(moved, n)
		}
	}
	copy(c.nodes[c.shard:end], moved)
	c.shard += len(filters)
	c.compute = slices.DeleteFunc(c.compute, func(s *loweredStage) bool {
		f, ok := s.nodes[0].(*filterNode)
		return ok && f.hoisted
	})
}

// splitCompute lists the stages that still run: each stage with a node
// above the shard, once, emitted as its own text. Leading row stages
// split off when the rows they pass along matter.
func (c *compiled) splitCompute() {
	var stages []*loweredStage
	for _, n := range c.nodes[c.shard:] {
		if s := c.stage(n); len(stages) == 0 || stages[len(stages)-1] != s {
			stages = append(stages, s)
		}
	}
	c.rowStages, c.compute = splitRowStages(stages)
}

// pushProjection is projection pushdown: a projection right above the
// shard's nodes becomes the SELECT, and its stage drops out of compute.
// It only applies to a first stage that reads shard rows as text, so
// not after row stages, and only when the stage after it names no
// meta-variable: that stage reads the projection's text, but once the
// projection is gone it comes first and reads the row's instead.
// Otherwise a jq first stage reads the payload, which the shard selects
// in raw's place.
func (c *compiled) pushProjection() {
	const pass = "projection-pushdown"
	if len(c.rowStages) > 0 {
		c.readsPayload = len(c.compute) > 0 && c.compute[0].cmd == "jq"
		return
	}
	if c.shard == len(c.nodes) {
		return
	}
	first := c.nodes[c.shard]
	if p, ok := first.(*projectNode); ok && len(c.compute) > 1 && c.compute[1].readsMeta {
		c.logf(pass, "%s stays in compute: %s names a meta-variable, which it would read from the row if it came first",
			c.label(p), c.label(c.compute[1].nodes[0]))
	} else if ok {
		c.selected = p
		c.shard++
		c.compute = c.compute[1:]
		c.logf(pass, "%s became the SELECT", c.label(p))
		return
	}
	if c.compute[0].cmd == "jq" {
		c.readsPayload = true
		c.logf(pass, "%s reads each row's payload: the SELECT returns it as raw", c.label(first))
	}
}

// fuseSort is sort/limit fusion: the shard ORDER BY and LIMIT for the
// direct-to-compute path (docs/plans/direct-to-compute.md §3). Compute's
// k-way merge keeps the shard's order, so the shard orders by the key
// compute merges on:
//
//	Mode A (no sort, or a pushed line count) → ts, direction from the
//	                                           head/tail, limit = the line count.
//	Mode B (a plain sort next to the shard)  → raw, direction from sort -r,
//	                                           limit only if already pushed.
//	Mode C (otherwise)                       → no shard order or limit.
func (c *compiled) fuseSort(orderDesc bool, limit int) {
	const pass = "sort-limit-fusion"
	idx := -1
	var sort *sortNode
	for i, s := range c.compute {
		if sn, ok := s.nodes[0].(*sortNode); ok {
			idx, sort = i, sn
		}
	}

	// A pushed line count runs below every compute stage, so a sort
	// after it sorts the survivors: `tail -N | … | sort` sorts the newest
	// N rows. The shard selects by ts, the line count's own order;
	// ordering it by raw would return the lexicographically smallest N.
	if sort == nil || limit > 0 {
		c.shardOrderBy, c.shardLimit = "ts", limit
		if orderDesc {
			c.shardOrderBy = "ts DESC"
		}
		switch {
		case sort != nil:
			c.logf(pass, "skipped %s: it sorts what a pushed line count kept, so the shard orders by ts", c.label(sort))
		case len(c.compute) > 0:
			c.logf(pass, "no sort in compute, so the shard orders by ts")
		}
		return
	}
	switch {
	case sort.keyed:
		// -n, -k, -f, -h, ... compare something other than the line's
		// bytes, which a shard ORDER BY raw can't match.
		c.logf(pass, "skipped %s: a flag changes its compare key, so no shard order", c.label(sort))
	case idx != 0:
		// A stage before the sort can rewrite the rows, so the shard's
		// raw order isn't the order sort compares.
		c.logf(pass, "skipped %s: it isn't next to the shard, so no shard order", c.label(sort))
	default:
		c.shardOrderBy, c.shardLimit = "raw", limit
		if sort.reverse {
			c.shardOrderBy = "raw DESC"
		}
		c.logf(pass, "%s became the shard's ORDER BY %s", c.label(sort), c.shardOrderBy)
	}
}

// fuseAggregate is aggregate recognition: the first compute stages, when
// they're `wc -l`, `sort -u` or `sort | uniq [-c]` (sort plain or -r),
// become the shard's GROUP BY, and the shard returns a count per line
// instead of the rows. Compute sums the shards' counts and prints the
// aggregate's text (QueryPlan.Aggregate), and the stages drop out.
//
// It needs every row in the shard's aggregate:
//
//   - no pushed line count, which compute applies to the merged rows
//     after the shard (`tail -n 500 | wc -l` counts 500, not 500 per
//     shard), and no row stages, which run in compute;
//   - a finite source: follow re-runs the stages on live rows;
//   - a log source: a bare trace tags its rows, which an aggregate
//     would drop.
//
// And like projection pushdown, it leaves the stage after it reading
// the aggregate's text, so that stage mustn't name a meta-variable.
func (c *compiled) fuseAggregate(limited bool) {
	const pass = "aggregate"
	if len(c.compute) == 0 {
		return
	}
	var fn string
	var n int // stages the aggregate covers
	var reverse bool
	switch first := c.compute[0].nodes[0].(type) {
	case *countNode:
		fn, n = AggregateLines, 1
	case *sortNode:
		if first.keyed {
			return
		}
		reverse = first.reverse
		var uniq *uniqNode
		if len(c.compute) > 1 {
			uniq, _ = c.compute[1].nodes[0].(*uniqNode)
		}
		switch {
		case uniq != nil && uniq.count && !first.unique:
			fn, n = AggregateCountDistinct, 2
		case uniq != nil && !uniq.count:
			fn, n = AggregateDistinct, 2
		case first.unique:
			fn, n = AggregateDistinct, 1
		default:
			return
		}
	default:
		return
	}
	label := c.label(c.compute[0].nodes[0])
	if n == 2 {
		label += " + " + c.label(c.compute[1].nodes[0])
	}
	switch {
	case limited:
		c.logf(pass, "skipped %s: a pushed line count is applied after the shards", label)
		return
	case len(c.rowStages) > 0:
		c.logf(pass, "skipped %s: row stages run in compute, before it", label)
		return
	case c.source.follow:
		c.logf(pass, "skipped %s: follow runs the stages on live rows", label)
		return
	case c.source.cmd == "trace" || c.source.cmd == "trace_spans":
		c.logf(pass, "skipped %s: a trace source", label)
		return
	case len(c.compute) > n && c.compute[n].readsMeta:
		c.logf(pass, "skipped %s: %s names a meta-variable, which it would read from the row if it came first",
			label, c.label(c.compute[n].nodes[0]))
		return
	}
	for _, s := range c.compute[:n] {
		c.aggregated = append(c.aggregated, s.nodes...)
	}
	c.shard += len(c.aggregated)
	c.compute = c.compute[n:]
	c.aggregate = fn
	switch {
	case fn == AggregateLines:
		c.shardOrderBy = ""
	case reverse:
		c.shardOrderBy = "raw DESC"
	default:
		c.shardOrderBy = "raw"
	}
	c.logf(pass, "%s became the shard's GROUP BY (%s)", label, fn)
}
