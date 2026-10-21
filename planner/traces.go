package planner

import "time"

// The aggregates behind a bare `trace`, the list of recent traces. A trace's
// lines sit on several shards (shards are per service), so the list takes two
// steps to be exact: AggregateTraceIDs finds the newest TraceListLimit
// trace IDs (the plan's own ShardSQL), then AggregateTraceList summarizes
// exactly those traces (QueryPlan.TraceStats). Asking each shard for its
// newest traces' summaries in one step would leave out the lines that sit on
// a shard where a trace is not among the newest.
const (
	AggregateTraceIDs  = "trace_ids"
	AggregateTraceList = "trace_list"

	TraceListLimit = 50
)

// traceRootRow selects the rows of a trace's root span: a span with no parent.
const traceRootRow = "span_name IS NOT NULL AND span_name <> '' AND (parent_span_id IS NULL OR parent_span_id = '')"

// TraceStatsQuery is the second step of the trace list.
type TraceStatsQuery struct {
	SQL       string
	Args      []any // the placeholders before the trace IDs
	Aggregate string
}

// WithIDs is the step's args for these trace IDs. The SQL has room for
// TraceListLimit IDs; fewer are padded with "", which the SQL excludes (see planTraceStats).
func (t *TraceStatsQuery) WithIDs(ids []string) []any {
	out := make([]any, 0, len(t.Args)+TraceListLimit)
	out = append(out, t.Args...)
	for i := 0; i < TraceListLimit; i++ {
		id := ""
		if i < len(ids) {
			id = ids[i]
		}
		out = append(out, id)
	}
	return out
}

// planTraceStats builds the second step. It is not scoped to the namespace
// or service the list is: the summary describes the whole trace.
func planTraceStats(customerID int64, start, end time.Time) *TraceStatsQuery {
	sql := newSQLBuilder()
	sql.addCustomer(customerID)
	sql.addTimeBounds(start, end)
	sql.addPredicate("trace_id <> ''") // the padding IDs are "", which lines without a trace ID also have
	addInPredicate(sql, "trace_id", make([]string, TraceListLimit))
	query := sql.spanQuery(AggregateTraceList)
	text, args, _ := query.ToSQLWithSources()
	return &TraceStatsQuery{SQL: text, Args: args[:len(args)-TraceListLimit], Aggregate: AggregateTraceList}
}
