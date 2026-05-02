package planner

import (
	"fmt"
	"strings"
	"time"
)

// sqlBuilder accumulates the parts of a shard SQL query.
//
// limitFrozen is set the moment any LIMIT pushdown lands (source
// head/tail, non-source head/tail, or an awk NR-window). The
// planner's stage loop uses the flag to stop pushing further
// predicates: post-limit filters belong on the *globally merged*
// top-N stream, not at the shard. Per-shard `LIMIT N WHERE filter`
// silently lets the cross-shard merge re-saturate the result. See
// docs/plans/cross-shard-post-limit-filter.md.
type sqlBuilder struct {
	rawExpr     string   // SQL expression for the raw column (default: "raw")
	predicates  []string // WHERE fragments, ANDed together
	orderBy     string   // e.g. "ts ASC" or "ts DESC"
	limit       int      // 0 = no limit
	offset      int      // 0 = no offset
	limitFrozen bool     // a LIMIT has been pinned; further stages must run in compute
}

func newSQLBuilder() *sqlBuilder {
	return &sqlBuilder{rawExpr: "raw"}
}

func (b *sqlBuilder) setSelect(rawExpr string) {
	b.rawExpr = rawExpr
}

func (b *sqlBuilder) addPredicate(pred string) {
	b.predicates = append(b.predicates, pred)
}

// addCustomer attaches the row-level security predicate. Empty
// customer is a no-op (no caller in production passes empty, but
// tests do). The escape mirrors what the old InjectCustomer did
// before being removed: doubled single-quotes prevent SQL injection
// via the customer name.
func (b *sqlBuilder) addCustomer(customer string) {
	if customer == "" {
		return
	}
	escaped := strings.ReplaceAll(customer, "'", "''")
	b.addPredicate(fmt.Sprintf("customer = '%s'", escaped))
}

// addTimeBounds attaches ts >= / ts <= predicates for the user's
// query window. Either bound may be zero-value, in which case it is
// skipped — Plan callers thread time.Time values through directly,
// and IsZero() is the sentinel for "no bound on this side." Real
// timestamps from epoch (time.Unix(0,0)) are not zero-value and do
// emit a (harmless) `ts >= TIMESTAMP '1970-01-01 00:00:00'` clause.
//
// The format is DuckDB-friendly. .999999 microseconds match the
// shard's TIMESTAMP column resolution (rows ingest via
// time.Unix(0, ts_unix_ns), so microsecond precision is the floor).
func (b *sqlBuilder) addTimeBounds(start, end time.Time) {
	if !start.IsZero() {
		b.addPredicate(fmt.Sprintf("ts >= TIMESTAMP '%s'", start.UTC().Format("2006-01-02 15:04:05.999999")))
	}
	if !end.IsZero() {
		b.addPredicate(fmt.Sprintf("ts <= TIMESTAMP '%s'", end.UTC().Format("2006-01-02 15:04:05.999999")))
	}
}

func (b *sqlBuilder) setOrderLimit(order string, limit int) {
	b.orderBy = order
	b.limit = limit
	b.limitFrozen = true
}

func (b *sqlBuilder) setLimitOffset(limit, offset int) {
	b.limit = limit
	b.offset = offset
	b.limitFrozen = true
}

// build produces the final SQL string.
// Always: SELECT line_num, {rawExpr} as raw FROM logs ...
func (b *sqlBuilder) build() string {
	return b.buildWith(b.orderBy, b.limit)
}

// buildWith produces SQL with the given orderBy + limit, ignoring the
// builder's own orderBy / limit fields. Used by the direct-to-compute
// path (see docs/plans/direct-to-compute.md §3) so the shard SQL
// carries the mode-classified "line_num" / "raw" ordering instead of
// the head/tail-derived "ts ASC".
func (b *sqlBuilder) buildWith(orderBy string, limit int) string {
	var sb strings.Builder

	if b.rawExpr == "raw" {
		sb.WriteString("SELECT line_num, raw FROM logs")
	} else {
		sb.WriteString(fmt.Sprintf("SELECT line_num, %s as raw FROM logs", b.rawExpr))
	}

	if len(b.predicates) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(b.predicates, " AND "))
	}

	if orderBy != "" {
		sb.WriteString(fmt.Sprintf(" ORDER BY %s", orderBy))
	}

	if limit > 0 {
		sb.WriteString(fmt.Sprintf(" LIMIT %d", limit))
	}

	if b.offset > 0 {
		sb.WriteString(fmt.Sprintf(" OFFSET %d", b.offset))
	}

	return sb.String()
}

