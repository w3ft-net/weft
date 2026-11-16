package dialect

import (
	"fmt"
	"strings"
)

// The trace and span aggregates a store runs, by the name the plan carries.
const (
	// AggSpans is each operation's duration histogram.
	AggSpans = "spans"
	// AggSpanSamples is one operation's slowest occurrences.
	AggSpanSamples = "span_samples"
	// AggSpanRows is every occurrence of every operation.
	AggSpanRows = "span_rows"
	// AggTraceIDs is the newest traces' IDs.
	AggTraceIDs = "trace_ids"
	// AggTraceList is a set of traces summarized.
	AggTraceList = "trace_list"
)

// SpanQuery is one trace or span aggregate over the logs table.
type SpanQuery struct {
	// Kind is one of the Agg constants.
	Kind  string
	Table string
	// Where is the WHERE clause, with its leading space, or empty.
	Where string
	// CountColumn names the column that carries each row's count.
	CountColumn string
	// TraceListLimit is how many traces AggTraceIDs names, SamplesLimit how many
	// occurrences AggSpanSamples returns.
	TraceListLimit, SamplesLimit int
	// LogGamma is the log of the bucket ratio of the duration histogram.
	LogGamma float64
}

// traceRootRow selects the rows of a trace's root span: a span with no parent.
const traceRootRow = "span_name IS NOT NULL AND span_name <> '' AND (parent_span_id IS NULL OR parent_span_id = '')"

const spanOccurrenceJSON = `json_object('name', span_name, 'namespace', namespace, 'service', service, ` +
	`'trace_id', trace_id, 'span_id', span_id, 'parent_id', parent_span_id, ` +
	`'dur_ns', duration_ns, 'ts_ns', epoch_ns(ts), 'level', level)::VARCHAR`

func (duckDB) RenderSpans(q SpanQuery) string {
	var b strings.Builder
	switch q.Kind {
	case AggSpanRows:
		fmt.Fprintf(&b, "SELECT %s AS raw, ts, 1 AS %s FROM %s", spanOccurrenceJSON, q.CountColumn, q.Table)
		b.WriteString(q.Where)
		b.WriteString(" ORDER BY raw")
	case AggTraceIDs:
		// The newest traces first: each shard names its own newest TraceListLimit
		// (the coordinator then asks for those traces' stats).
		fmt.Fprintf(&b, "SELECT raw, ts, 1 AS %s FROM (SELECT json_object('trace_id', trace_id, 'last_ns', epoch_ns(max(ts)))::VARCHAR AS raw, max(ts) AS ts FROM %s",
			q.CountColumn, q.Table)
		b.WriteString(q.Where)
		fmt.Fprintf(&b, " GROUP BY trace_id ORDER BY max(ts) DESC LIMIT %d) AS s ORDER BY raw", q.TraceListLimit)
	case AggTraceList:
		b.WriteString("SELECT json_object('trace_id', trace_id, " +
			"'first_ns', epoch_ns(min(ts)), 'last_ns', epoch_ns(max(ts)), " +
			"'lines', count(*), " +
			"'errors', count(*) FILTER (WHERE level = 'ERROR'), " +
			"'levels', count(*) FILTER (WHERE level <> ''), " +
			"'services', list(DISTINCT namespace || '/' || service), " +
			"'root', arg_min(span_name, ts) FILTER (WHERE " + traceRootRow + "), " +
			"'root_ns', epoch_ns(min(ts) FILTER (WHERE " + traceRootRow + ")))::VARCHAR AS raw, " +
			"min(ts) AS ts, 1 AS " + q.CountColumn + " FROM " + q.Table)
		b.WriteString(q.Where)
		b.WriteString(" GROUP BY trace_id ORDER BY raw")
	case AggSpanSamples:
		fmt.Fprintf(&b, "SELECT %s AS raw, ts, 1 AS %s FROM (SELECT * FROM %s", spanOccurrenceJSON, q.CountColumn, q.Table)
		b.WriteString(q.Where)
		fmt.Fprintf(&b, " ORDER BY duration_ns DESC LIMIT %d) AS s ORDER BY raw", q.SamplesLimit)
	default:
		// The same span name in two services is two different things, so
		// every group is a (name, namespace, service).
		const key = "span_name, namespace, service"
		const keyJSON = "'name', span_name, 'namespace', namespace, 'service', service"
		fmt.Fprintf(&b, "SELECT json_object(%s, 'bucket', bucket, "+
			"'count', count(*) FILTER (WHERE duration_ns > 0), "+
			"'errors', count(*) FILTER (WHERE level = 'ERROR'), "+
			"'levels', count(*) FILTER (WHERE level <> ''), "+
			"'total_ns', sum(duration_ns) FILTER (WHERE duration_ns > 0), "+
			"'min_ns', min(duration_ns) FILTER (WHERE duration_ns > 0), "+
			"'max_ns', max(duration_ns) FILTER (WHERE duration_ns > 0))::VARCHAR AS raw, "+
			"min(ts) AS ts, count(*) AS %s FROM (SELECT %s, level, duration_ns, ts, "+
			"CASE WHEN duration_ns > 0 THEN CAST(ceil(ln(duration_ns) / %v) AS BIGINT) END AS bucket FROM %s",
			keyJSON, q.CountColumn, key, q.LogGamma, q.Table)
		b.WriteString(q.Where)
		fmt.Fprintf(&b, ") AS s GROUP BY %s, bucket ORDER BY raw", key)
	}
	return b.String()
}
