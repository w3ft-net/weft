package planner

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	AggregateSpans       = "spans"
	AggregateSpanSamples = "span_samples"
	AggregateSpanRows    = "span_rows"
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
	}
	return ""
}

func (q *Query) spanQuery(kind string, byService bool) *Query {
	return &Query{from: q.from, predicates: q.predicates, spanKind: kind, spanByService: byService}
}

const spanOccurrenceJSON = `json_object('name', span_name, 'namespace', namespace, 'service', service, ` +
	`'trace_id', trace_id, 'span_id', span_id, 'parent_id', parent_span_id, ` +
	`'dur_ns', duration_ns, 'ts_ns', epoch_ns(ts), 'level', level)::VARCHAR`

func (q *Query) renderSpans(b *strings.Builder, onArg func(ArgSource)) {
	switch q.spanKind {
	case AggregateSpanRows:
		fmt.Fprintf(b, "SELECT %s AS raw, ts, 1 AS %s FROM %s", spanOccurrenceJSON, CountColumn, q.from.Table)
		q.renderWhere(b, onArg)
		b.WriteString(" ORDER BY raw")
	case AggregateSpanSamples:
		fmt.Fprintf(b, "SELECT %s AS raw, ts, 1 AS %s FROM (SELECT * FROM %s", spanOccurrenceJSON, CountColumn, q.from.Table)
		q.renderWhere(b, onArg)
		fmt.Fprintf(b, " ORDER BY duration_ns DESC LIMIT %d) AS s ORDER BY raw", SpanSamplesLimit)
	default:
		key, group := "span_name", "span_name"
		keyJSON := "'name', span_name"
		if q.spanByService {
			keyJSON += ", 'namespace', namespace, 'service', service"
			group += ", namespace, service"
			key += ", namespace, service"
		}
		fmt.Fprintf(b, "SELECT json_object(%s, 'bucket', bucket, "+
			"'count', count(*) FILTER (WHERE duration_ns > 0), "+
			"'errors', count(*) FILTER (WHERE level = 'ERROR'), "+
			"'total_ns', sum(duration_ns) FILTER (WHERE duration_ns > 0), "+
			"'min_ns', min(duration_ns) FILTER (WHERE duration_ns > 0), "+
			"'max_ns', max(duration_ns) FILTER (WHERE duration_ns > 0))::VARCHAR AS raw, "+
			"min(ts) AS ts, count(*) AS %s FROM (SELECT %s, level, duration_ns, ts, "+
			"CASE WHEN duration_ns > 0 THEN CAST(ceil(ln(duration_ns) / %v) AS BIGINT) END AS bucket FROM %s",
			keyJSON, CountColumn, key, spanLogGamma, q.from.Table)
		q.renderWhere(b, onArg)
		fmt.Fprintf(b, ") AS s GROUP BY %s, bucket ORDER BY raw", group)
	}
}

func planSpans(c *compiled, src sourceInfo, aggregate string, customerID int64, start, end time.Time, alertArgs []string, redirTarget string, redirAppend bool) (*QueryPlan, *compiled, error) {
	sql := newSQLBuilder()
	sql.addCustomer(customerID)
	sql.addTimeBounds(start, end)
	addSourcePredicates(sql, src)

	c.compute = c.stages
	c.aggregate = aggregate
	c.shardOrderBy = "raw"
	c.logf("span-aggregate", "%s runs in the shard; every stage runs in compute", aggregate)

	query := sql.spanQuery(aggregate, src.spansBy == "service")
	shardSQL, args, sources := query.ToSQLWithSources()
	return &QueryPlan{
		ShardSQL:         shardSQL,
		Args:             args,
		ArgSources:       sources,
		DirectShardSQL:   shardSQL,
		ComputeStages:    stageTexts(c.compute),
		SourceCmd:        src.cmd,
		SourceCustomerID: customerID,
		AlertArgs:        alertArgs,
		ShardOrderBy:     "raw",
		Aggregate:        aggregate,
		RedirectTarget:   redirTarget,
		RedirectAppend:   redirAppend,
	}, c, nil
}
