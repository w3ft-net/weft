package planner

import (
	"fmt"
	"strings"
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

// InjectCustomer attaches a customer = '...' predicate to the inner
// scan over `FROM logs`, regardless of whether the planner emitted a
// flat or nested query. The coordinator runs this on every shard SQL
// before dispatch — it is the row-level security boundary.
//
// Anchoring on the first `FROM logs` is what makes nested SQL safe:
// `SELECT … FROM (SELECT … FROM logs … LIMIT N) WHERE …` keeps the
// LIMIT bounded per-tenant when the customer predicate goes inside
// the subquery, not on the outer wrapper. A naive first-WHERE search
// would land on the outer in the no-inner-WHERE case and silently
// turn the per-shard LIMIT into a cross-tenant bottleneck.
func InjectCustomer(sql, customer string) string {
	pred := fmt.Sprintf("customer = '%s'", strings.ReplaceAll(customer, "'", "''"))
	const fromLogs = "FROM logs"
	idx := strings.Index(sql, fromLogs)
	if idx == -1 {
		return sql
	}
	rest := sql[idx+len(fromLogs):]
	// Find the next clause keyword that bounds the inner SELECT scope.
	// `)` marks the subquery boundary; nothing past it belongs to the
	// inner.
	end := len(rest)
	for _, kw := range []string{" WHERE ", " ORDER BY ", " LIMIT ", " OFFSET ", ")"} {
		if i := strings.Index(rest, kw); i != -1 && i < end {
			end = i
		}
	}
	head := sql[:idx+len(fromLogs)+end]
	tail := sql[idx+len(fromLogs)+end:]
	if strings.HasPrefix(tail, " WHERE ") {
		return head + " WHERE " + pred + " AND " + tail[len(" WHERE "):]
	}
	return head + " WHERE " + pred + tail
}
