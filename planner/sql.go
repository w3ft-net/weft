package planner

import (
	"fmt"
	"strings"
)

// sqlBuilder accumulates the parts of a shard SQL query.
type sqlBuilder struct {
	predicates []string // WHERE clause fragments, ANDed together
	orderBy    string   // e.g. "ts ASC" or "ts DESC"
	limit      int      // 0 = no limit
	offset     int      // 0 = no offset
}

func newSQLBuilder() *sqlBuilder {
	return &sqlBuilder{}
}

func (b *sqlBuilder) addPredicate(pred string) {
	b.predicates = append(b.predicates, pred)
}

func (b *sqlBuilder) setOrderLimit(order string, limit int) {
	b.orderBy = order
	b.limit = limit
}

func (b *sqlBuilder) setLimitOffset(limit, offset int) {
	b.limit = limit
	b.offset = offset
}

// build produces the final SQL string.
func (b *sqlBuilder) build() string {
	var sb strings.Builder
	sb.WriteString("SELECT * FROM logs")

	if len(b.predicates) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(b.predicates, " AND "))
	}

	if b.orderBy != "" {
		sb.WriteString(fmt.Sprintf(" ORDER BY %s", b.orderBy))
	}

	if b.limit > 0 {
		sb.WriteString(fmt.Sprintf(" LIMIT %d", b.limit))
	}

	if b.offset > 0 {
		sb.WriteString(fmt.Sprintf(" OFFSET %d", b.offset))
	}

	return sb.String()
}

// InjectCustomer prepends a customer = '...' predicate to an existing SQL
// query's WHERE clause. If the query has no WHERE clause, one is added.
// This is the row-level security boundary — the coordinator calls this on
// every query before sending it to a shard.
func InjectCustomer(sql, customer string) string {
	pred := fmt.Sprintf("customer = '%s'", strings.ReplaceAll(customer, "'", "''"))
	if strings.Contains(sql, " WHERE ") {
		return strings.Replace(sql, " WHERE ", " WHERE "+pred+" AND ", 1)
	}
	// No WHERE clause — insert before ORDER BY, LIMIT, or at the end.
	for _, kw := range []string{" ORDER BY ", " LIMIT "} {
		if idx := strings.Index(sql, kw); idx != -1 {
			return sql[:idx] + " WHERE " + pred + sql[idx:]
		}
	}
	return sql + " WHERE " + pred
}
