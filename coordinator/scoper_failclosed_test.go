//go:build !defaultshape

package coordinator

import (
	"context"
	"errors"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
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

	res, err := c.Execute(context.Background(), "cat /log/alpha/api/main.log", tr, hosted.Actor{Org: custID, User: ""})
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

// A scope narrower than a namespace (here one env) is enforced exactly: the
// query reads the intersection, so another env of the same namespace is refused
// and the scoped env is not.
func TestQuery_ScopeFinerThanNamespaceIsEnforcedExactly(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if _, err := db.FindOrCreateNamespace(custID, "alpha"); err != nil {
		t.Fatal(err)
	}
	set := shape.Set{{Customer: int64(custID), Namespace: shape.Value("alpha"), Env: shape.Value("prod")}}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db, Scoper: staticScoper{set}})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	run := func(pipeline string) error {
		res, err := c.Execute(context.Background(), pipeline, tr, hosted.Actor{Org: custID, User: ""})
		if err == nil && res != nil {
			d, derr := res.Drain(context.Background())
			if derr != nil {
				return derr
			}
			if d.Stderr != "" {
				return errors.New(d.Stderr)
			}
		}
		return err
	}
	if err := run("cat /log/alpha/staging/api/main.log"); err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Errorf("another env of the namespace: err = %v, want a refusal", err)
	}
	if err := run("cat /log/alpha/prod/api/main.log"); err != nil && strings.Contains(err.Error(), "unknown source") {
		t.Errorf("the scoped env was refused: %v", err)
	}
}

// otherActor is an identity the hosted adapter has never heard of.
type otherActor struct{ tenant int64 }

func (a otherActor) Partition() shape.Added { return shapetest.Added(a.tenant) }
func (otherActor) Subject() string          { return "svc-account" }

type recordingScoper struct{ got []tenancy.Actor }

func (r *recordingScoper) ReadScope(_ context.Context, a tenancy.Actor) (shape.Set, error) {
	r.got = append(r.got, a)
	return shape.Set{a.Partition().Coord()}, nil
}

// The Scoper sees the actor the caller authenticated as, whatever its type.
func TestQuery_ScoperReceivesTheCallersActor(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	rec := &recordingScoper{}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db, Scoper: rec})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	actor := otherActor{tenant: int64(custID)}
	res, err := c.Execute(context.Background(), "cat /log/alpha/api/main.log", tr, actor)
	if err == nil && res != nil {
		res.Drain(context.Background())
	}
	if len(rec.got) == 0 {
		t.Fatalf("Scoper never consulted (err=%v)", err)
	}
	for _, g := range rec.got {
		if g != tenancy.Actor(actor) {
			t.Errorf("Scoper saw %#v, want %#v", g, actor)
		}
	}
}
