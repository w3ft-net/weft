package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
)

// An empty customer identity must be refused before it reaches the planner.
// resolveCustomerID used to map "" to org 0, and addCustomer(0) emits no
// customer predicate, so a caller with no identity ran an unscoped query.
func TestExecute_EmptyCustomerIsRefused(t *testing.T) {
	db, _ := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	res, err := c.Execute(context.Background(), "cat vimgym.log", tr, 0, "")
	if err == nil {
		if res != nil {
			if d, derr := res.Drain(context.Background()); derr != nil {
				err = derr
			} else if d.Stderr != "" {
				t.Logf("stderr: %s", d.Stderr)
			}
		}
		if err == nil {
			t.Fatal("empty customer ran a query; want it refused before planning")
		}
	}
}
