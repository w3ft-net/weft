package planner

import "strings"

// plainSQL is the shard SQL for a query that selects the rows `where` selects,
// whole: the form every shard understands. meta adds the meta-variable
// columns, for a stage that reads one.
func plainSQL(where string, meta bool) string {
	cols := ""
	if meta {
		cols = ", " + strings.Join(MetaVarColumns, ", ")
	}
	return "SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no" + cols + " FROM logs WHERE " + where
}
