package planner

import "strings"

// plainSQL is the shard SQL for a query that selects the rows `where` selects,
// whole: the form every shard understands. metaCols are the meta-variable
// columns it also returns, for a stage that reads one.
func plainSQL(where string, metaCols ...string) string {
	cols := ""
	if len(metaCols) > 0 {
		cols = ", " + strings.Join(metaCols, ", ")
	}
	return "SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no" + cols + " FROM logs WHERE " + where
}
