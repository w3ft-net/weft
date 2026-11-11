package planner

import (
	"time"

	"github.com/graybeardcorp/weft/shape/shapetest"
)

// planBare plans with no tenant: no row-level-security predicate and no table
// hint. It is how the tests that look at everything but the tenant plan.
func planBare(pipeline, cwd string, start, end time.Time) (*QueryPlan, error) {
	plan, _, err := compileBare(pipeline, cwd, start, end)
	return plan, err
}

// tenantOfTest is tenant n as the planner holds it while planning.
func tenantOfTest(n int64) tenant {
	tn, err := tenantOf(shapetest.Tenant(n))
	if err != nil {
		panic(err)
	}
	return tn
}
