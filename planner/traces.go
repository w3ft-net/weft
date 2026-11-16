package planner

import (
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// The aggregates behind a bare `trace`, the list of recent traces. A trace's
// lines sit on several shards (shards are per service), so the list takes two
// steps to be exact: AggregateTraceIDs finds the newest TraceListLimit
// trace IDs (the plan's own ShardSQL), then AggregateTraceList summarizes
// exactly those traces (QueryPlan.TraceStats). Asking each shard for its
// newest traces' summaries in one step would leave out the lines that sit on
// a shard where a trace is not among the newest.
const (
	AggregateTraceIDs  = dialect.AggTraceIDs
	AggregateTraceList = dialect.AggTraceList

	TraceListLimit = 50
)

// TraceStatsQuery is the second step of the trace list.
type TraceStatsQuery struct {
	SQL string
	// ArchiveSQL is SQL spelled for the archive tier when it is written in a
	// different dialect than the live stores; empty otherwise.
	ArchiveSQL string
	Args       []any // the placeholders before the trace IDs
	Aggregate  string
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
//
// A user who may read only some namespaces (scope) is the exception: the
// summary then describes the trace as far as they can see it, or its service
// list and counts would reveal the namespaces they can't.
func planTraceStats(d dialect.Dialect, tn group, start, end time.Time, scope []string) *TraceStatsQuery {
	sql := newSQLBuilder(d)
	sql.addGroup(tn)
	sql.addTimeBounds(start, end)
	addInPredicate(sql, "namespace", scope)
	sql.addPredicate("trace_id <> ''") // the padding IDs are "", which lines without a trace ID also have
	addInPredicate(sql, "trace_id", make([]string, TraceListLimit))
	query := sql.spanQuery(AggregateTraceList)
	text, args, _ := query.ToSQLWithSources()
	return &TraceStatsQuery{SQL: text, Args: args[:len(args)-TraceListLimit], Aggregate: AggregateTraceList}
}
