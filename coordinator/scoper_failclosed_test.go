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

// staticScoper is a non-hosted adapter: a fixed policy, no store behind it.
type staticScoper struct{ set shape.Set }

func (s staticScoper) ReadScope(context.Context, tenancy.Actor) (shape.Set, error) {
	return s.set, nil
}

// A scope narrower than a namespace (here one env) is a grant the query path
// cannot enforce yet. It must refuse rather than widen the grant to the whole
// namespace.
func TestQuery_ScopeFinerThanNamespaceDenies(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if _, err := db.FindOrCreateNamespace(custID, "alpha"); err != nil {
		t.Fatal(err)
	}
	set := shape.Set{{Customer: int64(custID), Namespace: shape.Value("alpha"), Env: shape.Value("prod")}}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db, Scoper: staticScoper{set}})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	res, err := c.Execute(context.Background(), "cat /log/alpha/staging/api/main.log", tr, custID, "")
	if err == nil && res != nil {
		d, derr := res.Drain(context.Background())
		if derr != nil {
			err = derr
		} else if d.Stderr != "" {
			err = errors.New(d.Stderr)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err = %v, want a refusal of the unenforceable scope", err)
	}
}
