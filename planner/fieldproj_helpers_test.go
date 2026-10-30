package planner

import "strings"

// projectedSQL is the shard SQL for a first awk stage that reads only the
// fields r names: the lines of the rows `where` selects, each cut down to a
// record of just those fields (syntheticRecordSQL). meta adds the
// meta-variable columns, for a stage that reads one.
func projectedSQL(r fieldReads, where string, meta bool) string {
	cols := ""
	if meta {
		cols = ", " + strings.Join(MetaVarColumns, ", ")
	}
	return "SELECT " + syntheticRecordSQL(r) + " as raw, ts, event_id, line_no" + cols +
		" FROM (SELECT unnest(string_split(raw, chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no" + cols +
		" FROM logs WHERE " + where + ") AS t"
}
