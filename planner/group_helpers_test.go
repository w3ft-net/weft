package planner

import (
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"
)

// planBare plans with no group: no row-level-security predicate and no table
// hint. It is how the tests that look at everything but the group plan.
func planBare(pipeline, cwd string, start, end time.Time) (*QueryPlan, error) {
	plan, _, err := compileBare(pipeline, cwd, start, end)
	return plan, err
}

// groupOfTest is group n as the planner holds it while planning.
func groupOfTest(n int64) group {
	tn, err := groupOf(shapetest.Group(n))
	if err != nil {
		panic(err)
	}
	return tn
}
