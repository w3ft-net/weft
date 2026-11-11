// Package shapetest is what tests use to name a tenant without naming the
// column that holds it. Which columns a build's partition has is its shape, so a
// test that wrote `Customer: 7` would not compile under another shape; these
// build the same things from the layout.
package shapetest

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/weft/logtable"
	"github.com/graybeardcorp/weft/shape"
)

// Partition is tenant n's partition values: n in every partition column of the
// build's shape (none, for a shape without a tenant).
func Partition(n int64) []string {
	vals := make([]string, len(shape.BuildLayout().Columns))
	for i := range vals {
		vals[i] = strconv.FormatInt(n, 10)
	}
	return vals
}

// Tenant is the read scope of tenant n: everything in its partition. A shape
// without a partition column has no tenant, so it is the one open tuple.
func Tenant(n int64) shape.Set {
	c, err := shape.CoordFromPartition(Partition(n))
	if err != nil {
		panic(fmt.Sprintf("shapetest.Tenant(%d): %v", n, err))
	}
	return shape.Set{c}
}

// Table is the logical table of tenant n's source (namespace, service, kind):
// logtable.Name over the tenant's typed partition, whichever shape the build has.
func Table(n int64, namespace, service, kind string) string {
	typed, err := shape.BuildLayout().Parse(Partition(n))
	if err != nil {
		panic(fmt.Sprintf("shapetest.Table(%d): %v", n, err))
	}
	return logtable.Name(typed, namespace, service, kind)
}

// Predicate is the SQL the build's shape adds for a tenant, as it renders in a
// WHERE clause and followed by " AND ": `customer_id = CAST(? AS BIGINT) AND `
// in the hosted shape, and "" in a shape without a partition column.
func Predicate() string {
	var parts []string
	for _, c := range shape.BuildLayout().Columns {
		parts = append(parts, c.Name+" = CAST(? AS "+c.Type.SQL()+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " AND ") + " AND "
}

// Where is the whole WHERE clause of a query whose only predicate is the
// tenant's, with its leading space: " WHERE customer_id = CAST(? AS BIGINT)"
// in the hosted shape, and "" in a shape without a partition column.
func Where() string {
	if p := Predicate(); p != "" {
		return " WHERE " + strings.TrimSuffix(p, " AND ")
	}
	return ""
}

// Args are the leading arguments the tenant predicate takes for tenant n, as
// text: one per partition column.
func Args(n int64) []any {
	var out []any
	for _, v := range Partition(n) {
		out = append(out, v)
	}
	return out
}
