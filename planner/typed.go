package planner

import (
	"slices"
	"strconv"
	"strings"

	"github.com/w3ft-net/weft/logtable"
)

// A typed leaf is a comparison whose SQL gets simpler, and tighter, if the
// shard has said every row of the source holds a number at its locator. The
// planner stays pure: it emits the plan it
// can prove from the text alone, and beside it a variant for shards whose
// facts allow it. The coordinator picks per shard; a fact flipping never
// forces a relowering.
type typedLeaf struct {
	locator string // "$N" for awk field N, ".key" for a top-level payload key
	base    string // the predicate text in the plan
	typed   string // what replaces it on a shard where locator is typed
}

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
	FieldSQL       string // the plan's FieldSQL with the same swap; empty if it has none
	// Exact is the whole plan for a query whose every shard has the facts
	// Requires: the typed comparisons are exact there, so it needs no more
	// compute than the pipeline's other stages do. Its Typed is nil.
	Exact *QueryPlan
}

// typedVariant derives the typed plan from the base SQL by swapping each
// typed leaf's predicate for its typed form. It declines (nil) unless the
// source is one stream, and unless every leaf's text occurs exactly once in
// both SQL strings, so a swap can't land on the wrong text.
func typedVariant(c *compiled, src sourceInfo, tn group, shardSQL, directSQL, fieldSQL string) *TypedPlan {
	ns, svc, kind, ok := src.single()
	if !ok || src.follow {
		return nil
	}
	var leaves []typedLeaf
	for _, n := range c.nodes[:c.shard] {
		if f, ok := n.(*filterNode); ok && !f.hoisted {
			leaves = append(leaves, f.typed...)
		}
	}
	// An awk aggregate that stays in compute becomes the shard's GROUP BY on
	// shards with its facts. It swaps no SQL: its plan is the Exact one.
	var aggFacts []string
	if a, _ := leadingAggregate(c.compute); a != nil {
		aggFacts = a.agg.facts()
	}
	if len(leaves) == 0 && len(aggFacts) == 0 {
		return nil
	}
	swap := func(sql string) (string, bool) {
		for _, l := range leaves {
			if strings.Count(sql, l.base) != 1 {
				return "", false
			}
		}
		for _, l := range leaves {
			sql = strings.Replace(sql, l.base, l.typed, 1)
		}
		return sql, true
	}
	shard, ok := swap(shardSQL)
	if !ok {
		return nil
	}
	direct, ok := swap(directSQL)
	if !ok {
		return nil
	}
	if fieldSQL != "" {
		if fieldSQL, ok = swap(fieldSQL); !ok {
			return nil
		}
	}
	var requires []string
	for _, l := range leaves {
		if !slices.Contains(requires, l.locator) {
			requires = append(requires, l.locator)
		}
	}
	for _, loc := range aggFacts {
		if !slices.Contains(requires, loc) {
			requires = append(requires, loc)
		}
	}
	slices.Sort(requires)
	return &TypedPlan{
		Requires: requires, Table: logtable.Name(tn.typed, ns, svc, kind), Env: src.envValue(),
		ShardSQL: shard, DirectShardSQL: direct, FieldSQL: fieldSQL,
	}
}

// typedNumber is a float as a SQL literal.
func typedNumber(n float64) string { return strconv.FormatFloat(n, 'g', -1, 64) }
