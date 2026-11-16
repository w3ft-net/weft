package planner

import (
	"slices"
	"strconv"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/logtable"
)

// A typed choice (sqlExpr) is a comparison whose SQL gets simpler, and tighter,
// if the shard has said every row of the source holds a number at its locator.
// The planner stays pure: it emits the plan it can prove from the text alone,
// and beside it a variant for shards whose facts allow it. The coordinator picks
// per shard; a fact flipping never forces a relowering.

// TypedPlan is the QueryPlan's shard SQL for a shard whose facts say every
// locator in Requires is all numbers in the one stream below. Its args, stages
// and merge are the plan's own: only the shard's WHERE differs, and only by
// being narrower (it never drops a row the base SQL keeps and the stage
// would), so compute's stages still run unchanged on what comes back.
type TypedPlan struct {
	Requires []string
	// Table is the one stream's logical table (logtable.Name): with Env, what a
	// shard's field facts are keyed by.
	Table          string
	Env            string
	ShardSQL       string
	DirectShardSQL string
	FieldSQL       string // the plan's FieldSQL in its typed form; empty if it has none
	// Exact is the whole plan for a query whose every shard has the facts
	// Requires: the typed comparisons are exact there, so it needs no more
	// compute than the pipeline's other stages do. Its Typed is nil.
	Exact *QueryPlan
}

// typedVariant is the typed plan: the plan's queries with every typed choice in
// their typed form. It declines (nil) unless the source is one stream, and
// when the dialect can't spell a typed form. fieldQ is nil when the plan has
// no field form.
func typedVariant(d dialect.Dialect, c *compiled, src sourceInfo, tn group, shardQ, directQ, fieldQ *Query) *TypedPlan {
	ns, svc, kind, ok := src.single()
	if !ok || src.follow {
		return nil
	}
	var requires []string
	for _, n := range c.nodes[:c.shard] {
		f, ok := n.(*filterNode)
		if !ok || f.hoisted {
			continue
		}
		for _, ch := range typedChoices(f.pred) {
			if !supported(ch.typed, d) {
				return nil
			}
			requires = append(requires, ch.locator)
		}
	}
	// An awk aggregate that stays in compute becomes the shard's GROUP BY on
	// shards with its facts. It swaps no SQL: its plan is the Exact one.
	if a, _ := leadingAggregate(c.compute); a != nil {
		requires = append(requires, a.agg.facts()...)
	}
	if len(requires) == 0 {
		return nil
	}
	slices.Sort(requires)
	requires = slices.Compact(requires)

	plan := &TypedPlan{Requires: requires, Table: logtable.Name(tn.typed, ns, svc, kind), Env: src.envValue()}
	plan.ShardSQL, _ = shardQ.typedForm().ToSQL()
	plan.DirectShardSQL, _ = directQ.typedForm().ToSQL()
	if fieldQ != nil {
		plan.FieldSQL, _ = fieldQ.typedForm().ToSQL()
	}
	return plan
}

// typedComparison is col compared with a number, for a shard where col is known
// to hold a finite decimal in every row: the cast alone is the comparison.
func typedComparison(col sqlExpr, op dialect.CmpOp, n float64) sqlExpr {
	return cmpExpr{op, tryCastDouble{col}, numLit(typedNumber(n))}
}

// typedNumber is a float as a SQL literal.
func typedNumber(n float64) string { return strconv.FormatFloat(n, 'g', -1, 64) }
