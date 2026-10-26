package shard

import (
	"strings"
	"time"
)

// A pushed-down query's time bounds, exactly as the planner words them
// (planner.Query.addTimeBounds); the planner's tests pin that wording to this.
const (
	tsLowerBound = "ts >= CAST(? AS TIMESTAMP)"
	tsUpperBound = "ts <= CAST(? AS TIMESTAMP)"

	// The planner's timestamp argument format (planner.formatTS).
	tsArgLayout = "2006-01-02 15:04:05.999999"

	// An archive that doesn't record its bucket width (PartitionMinutes) was
	// split at day, hour, 10-minute or minute granularity, and a row's _part is
	// the start of its bucket. The coarsest bucket is a day, so a row at or
	// after t is in a partition that starts after t minus a day.
	coarsestLegacyPartition = 24 * time.Hour
)

// PruneColdPartitions restates a pushed-down query's time bounds on _part, the
// partition column of a hive-partitioned archive, so DuckDB skips the files
// outside them. It filters on ts, which DuckDB can't turn into a partition
// filter itself, and a fresh DuckDB (the cold-shard Lambda starts one per
// query) otherwise opens every file's footer to find that out.
//
// With the archive's bucket width in its path the low side is the bucket
// holding the bound; without one it is a day earlier. Each bound F becomes
// (F AND _part ...) where F implies the added condition for every row, so the
// rewrite never changes which rows match. A bound it can't read is left as it
// was.
func PruneColdPartitions(sql string, args []string, parquetPath string) string {
	minutes, _ := PartitionMinutes(parquetPath)
	var out strings.Builder
	rest := sql
	consumed := 0 // bytes of sql already copied to out
	for {
		i, bound := nextTimeBound(rest)
		if i < 0 {
			break
		}
		at := consumed + i
		n := len(tsLowerBound)
		if bound == tsUpperBound {
			n = len(tsUpperBound)
		}
		out.WriteString(sql[consumed:at])
		frag := sql[at : at+n]
		if t, ok := timeArgAt(sql, at, args); ok && !precededByIdentifier(sql, at) {
			frag = "(" + frag + " AND " + partBound(bound, t, minutes) + ")"
		}
		out.WriteString(frag)
		consumed = at + n
		rest = sql[consumed:]
	}
	out.WriteString(sql[consumed:])
	return out.String()
}

// nextTimeBound finds the earliest time-bound fragment in s.
func nextTimeBound(s string) (int, string) {
	lo, hi := strings.Index(s, tsLowerBound), strings.Index(s, tsUpperBound)
	switch {
	case lo < 0 && hi < 0:
		return -1, ""
	case hi < 0 || (lo >= 0 && lo < hi):
		return lo, tsLowerBound
	default:
		return hi, tsUpperBound
	}
}

// partBound is the _part condition implied by a time bound; minutes is the
// archive's bucket width, 0 when it doesn't record one.
func partBound(bound string, t time.Time, minutes int) string {
	switch {
	case bound == tsUpperBound:
		return "_part <= '" + t.Format(tsArgLayout) + "'"
	case minutes > 0:
		return "_part >= '" + BucketStart(t, minutes).Format(tsArgLayout) + "'"
	default:
		return "_part > '" + t.Add(-coarsestLegacyPartition).Format(tsArgLayout) + "'"
	}
}

// timeArgAt parses the argument bound to the placeholder in the fragment at
// sql[at:]: the nth `?` outside quotes, counting from the start.
func timeArgAt(sql string, at int, args []string) (time.Time, bool) {
	n := 0
	var quote byte
	for i := 0; i < at; i++ {
		switch c := sql[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '?':
			n++
		}
	}
	if n >= len(args) {
		return time.Time{}, false
	}
	t, err := time.Parse(tsArgLayout, args[n])
	return t, err == nil
}

// precededByIdentifier reports whether the fragment is the tail of a longer
// name (pts >= ...), not the ts column.
func precededByIdentifier(sql string, at int) bool {
	if at == 0 {
		return false
	}
	c := sql[at-1]
	return c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
