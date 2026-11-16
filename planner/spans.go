package planner

import (
	"math"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

const (
	AggregateSpans       = dialect.AggSpans
	AggregateSpanSamples = dialect.AggSpanSamples
	AggregateSpanRows    = dialect.AggSpanRows
)

const (
	SpanSamplesLimit = 50

	spanRelativeError = 0.01
)

var spanLogGamma = math.Log((1 + spanRelativeError) / (1 - spanRelativeError))

func SpanBucketValue(bucket int64) float64 {
	gamma := math.Exp(spanLogGamma)
	return 2 * math.Pow(gamma, float64(bucket)) / (gamma + 1)
}

func spanAggregateOf(src sourceInfo) string {
	switch {
	case src.cmd == "trace_spans" && src.samples:
		return AggregateSpanSamples
	case src.cmd == "trace_spans":
		return AggregateSpans
	case src.cmd == "trace" && src.spansOnly:
		return AggregateSpanRows
	case src.cmd == "trace_list":
		return AggregateTraceIDs
	}
	return ""
}

func (q *Query) spanQuery(kind string) *Query {
	return &Query{d: q.d, from: q.from, predicates: q.predicates, spanKind: kind}
}

// renderSpans is the store's SQL for the span or trace aggregate, over q's
// table and WHERE.
func (q *Query) renderSpans(b *strings.Builder, onArg func(ArgSource)) {
	var where strings.Builder
	q.renderWhere(&where, onArg)
	b.WriteString(q.d.RenderSpans(dialect.SpanQuery{
		Kind:           q.spanKind,
		Table:          q.from.Table,
		Where:          where.String(),
		CountColumn:    CountColumn,
		TraceListLimit: TraceListLimit,
		SamplesLimit:   SpanSamplesLimit,
		LogGamma:       spanLogGamma,
	}))
}

func planSpans(c *compiled, src sourceInfo, aggregate string, tn group, start, end time.Time, alertArgs []string, redirTarget string, redirAppend bool) (*QueryPlan, *compiled, error) {
	sql := newSQLBuilder(c.d)
	sql.addGroup(tn)
	sql.addTimeBounds(start, end)
	addSourcePredicates(sql, src)

	c.compute = c.stages
	c.aggregate = aggregate
	c.shardOrderBy = "raw"
	c.logf("span-aggregate", "%s runs in the shard; every stage runs in compute", aggregate)

	query := sql.spanQuery(aggregate)
	shardSQL, args, sources := query.ToSQLWithSources()
	var traceStats *TraceStatsQuery
	if aggregate == AggregateTraceIDs {
		traceStats = planTraceStats(c.d, tn, start, end, src.traceScope())
	}
	return &QueryPlan{
		TraceStats:      traceStats,
		ShardSQL:        shardSQL,
		Args:            args,
		ArgSources:      sources,
		DirectShardSQL:  shardSQL,
		ComputeStages:   stageTexts(c.compute),
		SourceCmd:       src.cmd,
		SourcePartition: tn.values,
		SourceNamespace: routeNamespace(src),
		Coords:          src.coord,
		SourceService:   routeService(src),
		AlertArgs:       alertArgs,
		ShardOrderBy:    "raw",
		Aggregate:       aggregate,
		RedirectTarget:  redirTarget,
		RedirectAppend:  redirAppend,
	}, c, nil
}
