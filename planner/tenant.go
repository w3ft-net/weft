package planner

import (
	"errors"
	"fmt"

	coords "github.com/graybeardcorp/weft/shape"
)

// tenant is the partition a plan is scoped to: the values of the shape's
// partition columns. Which columns those are is the build's shape, so the
// planner names none: the row-level-security predicate is one equality per
// column of the layout, and the default shape, which has none, has no
// predicate at all.
//
// The zero tenant carries no values and, in a shape that has partition
// columns, adds no predicate. Explain and analyze plan that way, because they
// show how a pipeline is planned, not what a tenant may read; no query that
// reads data does.
type tenant struct {
	values []string // the partition as text, in layout order
	typed  []any    // the same, typed per the layout, for table names
}

// tenantOf is the partition scope names. An empty scope has none: it reads
// nothing, which the plan reports as an unknown source once the pipeline has
// parsed, so that is not an error here.
func tenantOf(scope coords.Set) (tenant, error) {
	values, err := scope.Partition()
	switch {
	case err == nil:
	case errors.Is(err, coords.ErrNoScope):
		return tenant{}, nil
	default:
		return tenant{}, fmt.Errorf("plan: %w", err)
	}
	typed, err := coords.BuildLayout().Parse(values)
	if err != nil {
		return tenant{}, fmt.Errorf("plan: %w", err)
	}
	return tenant{values: values, typed: typed}, nil
}

// complete reports whether the tenant names a value for every partition
// column of the layout. The zero tenant does, in a shape with no such columns.
func (t tenant) complete() bool {
	return len(t.values) == len(coords.BuildLayout().Columns)
}

// coord is the tuple that names the tenant's partition with every other
// column open: the zero tuple when the tenant is not complete.
func (t tenant) coord() coords.Coord {
	if !t.complete() {
		return coords.Coord{}
	}
	c, err := coords.CoordFromPartition(t.values)
	if err != nil {
		return coords.Coord{}
	}
	return c
}
