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
	meta         MetaRead
	shardOrderBy string
	shardLimit   int
	// group is the shard-side grouping that replaced the first compute
	// stages, if any; aggregated is those stages' nodes. aggregate is a
	// trace or span source's own aggregate (spans.go).
	group    *GroupBySpec
	groupKey string    // the SQL an awk aggregate groups by, in place of raw
	groupAgg *keyedAgg // the awk aggregate the group is, if it is one
	// aggNotes is what Explain says of an aggregated node that isn't itself
	// the GROUP BY.
	aggNotes map[node]string
	// fieldSpec, when set, is the fields the first compute stage reads
	// (projectFields), for the field form of the query that sends their
	// values in place of each line (QueryPlan.FieldSQL).
	fieldSpec *fieldReads
	// payloadSelect, when set, is the SQL for a payload of just the keys the
	// first compute stage (a jq) reads (projectPayload), in place of all of it.
	payloadSelect string
	aggregate     string
	aggregated    []node
	passes        []passStep
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
	case *aggregateNode:
		return aggregateKind(n.(*aggregateNode).agg.op)
	}
	return "Opaque"
}

// aggregateKind is what Explain calls an aggregate node.
func aggregateKind(op aggOp) string {
	switch op {
	case aggSum:
		return "Sum"
	case aggMax:
		return "Max"
	case aggMin:
		return "Min"
	}
	return "Tally"
}

// pushFilters is filter pushdown. Starting from the scan, filters and
// limits go into the shard until the first node that can't:
//
//   - a filter or limit above a limit stays put: a line count ends
//     predicate pushdown (`tail -n 500 | grep x` filters the last 500
//     rows), and the
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
	case c.source.cmd == "trace" || c.source.cmd == "trace_spans" || c.source.cmd == "trace_list":
		c.logf(pass, "skipped %s: a trace source", c.label(filters[0]))
		return
	case len(sorts) == 0 && end < len(c.nodes) && c.stage(c.nodes[end]).meta.Reads():
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
	if p, ok := first.(*projectNode); ok && len(c.compute) > 1 && c.compute[1].meta.Reads() {
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
// direct-to-compute path. Compute's
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
// they're `wc -l`, `sort -u [| wc -l]` or `sort | uniq [-c]` (sort plain
// or -r, and `uniq | wc -l` counts the distinct lines),
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
	var render GroupRender
	var n int // stages the aggregate covers
	var reverse bool
	var ordered bool
	var limit int
	var key string
	var agg *keyedAgg
	var order *ValueOrder
	var first node = c.aboveShard(c.compute[0])
	skipped := 0 // sorts ahead of an aggregate that doesn't depend on row order
	if an, dropped := leadingAggregate(c.compute); an != nil {
		first, skipped = an, dropped
	}
	switch first := first.(type) {
	case *countNode:
		render, n = RenderTotal, 1
		if first.bytes {
			render = RenderBytes
		}
	case *aggregateNode:
		if len(first.agg.facts()) > 0 && !first.typed {
			c.logf(pass, "skipped %s: $%d is not known to hold only %s", c.label(first), first.agg.field, first.agg.holds())
			return
		}
		render, n, key, agg = first.agg.render(), 1+skipped, first.agg.key, &first.agg
		// The aggregate's own lines, ordered by value and cut to a count: the
		// `sort -k2 -rn | head -n N` after it.
		if rest := c.compute[n:]; len(rest) > 0 && len(rest[0].nodes) == 1 {
			if sn, ok := rest[0].nodes[0].(*sortNode); ok && sn.order != nil {
				order, n = sn.order, n+1
				if len(rest) > 1 && len(rest[1].nodes) == 1 {
					if hd, ok := rest[1].nodes[0].(*limitNode); ok && hd.order == "ts ASC" && hd.offset == 0 && hd.n > 0 {
						limit, n = hd.n, n+1
					}
				}
			}
		}
	case *sortNode:
		if first.keyed {
			r, ok := numericEnd(first, c.compute[1:])
			if !ok {
				return
			}
			render, n = r, 2
			break
		}
		reverse = first.reverse
		var uniq *uniqNode
		if len(c.compute) > 1 {
			uniq, _ = c.compute[1].nodes[0].(*uniqNode)
		}
		switch {
		case uniq != nil && uniq.count && !first.unique:
			render, n = RenderCountLine, 2
			ordered, limit, n = topN(c.compute[n:], n)
		case uniq != nil && !uniq.count:
			render, n = RenderLine, 2
		case first.unique:
			render, n = RenderLine, 1
		default:
			return
		}
		// A count of the distinct lines is the number of groups.
		if render == RenderLine && len(c.compute) > n {
			if cn, ok := c.compute[n].nodes[0].(*countNode); ok && !cn.bytes {
				render, n = RenderDistinctTotal, n+1
			}
		}
	default:
		return
	}
	label := c.label(c.aboveShard(c.compute[0]))
	for _, s := range c.compute[1:n] {
		label += " + " + c.label(s.nodes[0])
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
	case key != "" && c.selected != nil:
		c.logf(pass, "skipped %s: the key reads the text %s projected, not the row", label, c.label(c.selected))
		return
	case c.source.cmd == "trace" || c.source.cmd == "trace_spans" || c.source.cmd == "trace_list":
		c.logf(pass, "skipped %s: a trace source", label)
		return
	case len(c.compute) > n && c.compute[n].meta.Reads():
		c.logf(pass, "skipped %s: %s names a meta-variable, which it would read from the row if it came first",
			label, c.label(c.compute[n].nodes[0]))
		return
	}
	for _, s := range c.compute[:n] {
		for _, nd := range s.nodes {
			if slices.Contains(c.nodes[c.shard:], nd) {
				c.aggregated = append(c.aggregated, nd)
			}
		}
	}
	c.shard += len(c.aggregated)
	c.compute = c.compute[n:]
	c.group = &GroupBySpec{Render: render, ByCount: ordered, Limit: limit, Order: order}
	c.noteAggregate(skipped, order != nil, limit)
	c.groupKey, c.groupAgg = key, agg
	switch {
	case render == RenderTotal || render == RenderBytes || render == RenderNumericMax || render == RenderNumericMin:
		c.shardOrderBy = ""
	case reverse:
		c.shardOrderBy = "raw DESC"
	default:
		c.shardOrderBy = "raw"
	}
	c.logf(pass, "%s became the shard's GROUP BY (%s)", label, render)
}

// aboveShard is a stage's first node that the shard doesn't run. A stage
// can have nodes on both sides of the boundary: grep -c is a filter that
// pushed and a count that didn't.
func (c *compiled) aboveShard(s *loweredStage) node {
	for _, n := range s.nodes {
		if slices.Contains(c.nodes[c.shard:], n) {
			return n
		}
	}
	return s.nodes[0]
}

// numericEnd is whether a sort -n (or -rn) with no other flag is followed by
// a head or tail of exactly one line: the line it puts first or last is the
// one with the greatest or least number. Ties go to the greater line in the
// same direction, since sort's last-resort compare reverses with -r.
func numericEnd(sort *sortNode, rest []*loweredStage) (GroupRender, bool) {
	if !sort.numeric || sort.unique || len(rest) == 0 || len(rest[0].nodes) != 1 {
		return "", false
	}
	end, ok := rest[0].nodes[0].(*limitNode)
	if !ok || end.n != 1 || end.offset != 0 {
		return "", false
	}
	last := end.order == "ts DESC"
	if last != sort.reverse {
		return RenderNumericMax, true
	}
	return RenderNumericMin, true
}

// topN is the ordering after a sort | uniq -c: `sort -rn` (highest count
// first), then optionally `head -n N`. It returns whether the sort is
// there, the line count (0 keeps every group), and the stage count the
// aggregate now covers, n plus what it took. A head -n 0 stays a stage:
// a limit of 0 means every group.
func topN(rest []*loweredStage, n int) (ordered bool, limit, covered int) {
	if len(rest) == 0 || len(rest[0].nodes) != 1 {
		return false, 0, n
	}
	sort, ok := rest[0].nodes[0].(*sortNode)
	if !ok || !sort.numeric || !sort.reverse || sort.unique {
		return false, 0, n
	}
	if len(rest) > 1 && len(rest[1].nodes) == 1 {
		if head, ok := rest[1].nodes[0].(*limitNode); ok && head.order == "ts ASC" && head.offset == 0 && head.n > 0 {
			return true, head.n, n + 2
		}
	}
	return true, 0, n + 1
}

// leadingAggregate is the awk aggregate that starts the compute stages: the
// first stage, or the second behind a sort, which a count or a sum doesn't depend
// on (a sum is exact, so adding in any order is the same). A max or min is not
// that: the row it keeps from a tie is the first one met, in the sort's order.
// It returns the aggregate and how many sorts come before it, nil if none.
func leadingAggregate(compute []*loweredStage) (*aggregateNode, int) {
	if len(compute) == 0 || len(compute[0].nodes) != 1 {
		return nil, 0
	}
	switch n := compute[0].nodes[0].(type) {
	case *aggregateNode:
		return n, 0
	case *sortNode:
		if !n.plain || len(compute) < 2 || len(compute[1].nodes) != 1 {
			return nil, 0
		}
		if an, ok := compute[1].nodes[0].(*aggregateNode); ok && (an.agg.op == aggCount || an.agg.op == aggSum) {
			return an, 1
		}
	}
	return nil, 0
}

// noteAggregate says what the aggregate pass did to the stages around the
// aggregate, for Explain: the sorts it dropped, and the order and limit it kept
// for compute to apply.
func (c *compiled) noteAggregate(dropped int, ordered bool, limit int) {
	if c.aggNotes == nil {
		c.aggNotes = map[node]string{}
	}
	i := 0
	if dropped > 0 {
		c.aggNotes[c.aggregated[0]] = "dropped: the aggregate does not depend on row order"
		i = 2
	} else {
		i = 1
	}
	if ordered && i < len(c.aggregated) {
		c.aggNotes[c.aggregated[i]] = "applied to the aggregate's lines in compute"
		if limit > 0 && i+1 < len(c.aggregated) {
			c.aggNotes[c.aggregated[i+1]] = "keeps the first of them"
		}
	}
}
