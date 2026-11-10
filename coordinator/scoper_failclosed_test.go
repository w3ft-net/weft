package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/tenancy"
)

type failingScoper struct{}

func (failingScoper) ReadScope(context.Context, tenancy.Actor) (shape.Set, error) {
	return nil, errors.New("scoper down")
}

// A Scoper error denies the request. There is no fallback scope.
func TestQuery_ScoperErrorDenies(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db, Scoper: failingScoper{}})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	res, err := c.Execute(context.Background(), "cat /log/alpha/api/main.log", tr, custID, "")
	if err == nil && res != nil {
		d, derr := res.Drain(context.Background())
		if derr != nil {
			err = derr
		} else if d.Stderr != "" {
			err = errors.New(d.Stderr)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "scoper down") {
		t.Fatalf("err = %v, want the Scoper's refusal", err)
	}
}
