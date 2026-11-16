package planner

import (
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"
)

// goldenPlan plans for customer 7 with today's API. It is the only place the
// golden test names the planner entry point.
func goldenPlan(pipeline string, start, end time.Time) (*QueryPlan, error) {
	return Plan(pipeline, shapetest.Group(7), "", start, end)
}
