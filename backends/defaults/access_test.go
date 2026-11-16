package defaults

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// This adapter names no group. Installed by mistake under a shape that has
// one (a hosted build that forgot its adapter), its scope must be refused, not
// read as everyone's data.
func TestScoper_UnderAGroupShapeTheScopeIsRefused(t *testing.T) {
	shapetest.NeedsGroup(t)
	scope, err := OpenScoper{}.ReadScope(context.Background(), OpenPrincipal{})
	if err != nil {
		return // refused by the scoper itself: also closed
	}
	if _, err := planner.Plan(`cat /log/ns/svc/main.log`, scope, "", time.Time{}, time.Time{}); err == nil {
		t.Fatal("a scope that names no group planned under a shape with one")
	}
}

// The adapter of a build with one group reads the whole of the principal's
// partition and nothing else.
func TestScoper_ReadsThePrincipalsWholePartition(t *testing.T) {
	a := OpenPrincipal{User: "u"}
	got, err := OpenScoper{}.ReadScope(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a.Partition().Coord() {
		t.Errorf("ReadScope = %+v, want the one whole-partition tuple", got)
	}
	if _, err := (OpenScoper{}).ReadScope(context.Background(), nil); !errors.Is(err, identity.ErrDenied) {
		t.Errorf("no principal: err = %v, want a denial", err)
	}
	ns, err := OpenScoper{}.NamespaceScope(context.Background(), shape.Added{}, "any")
	if err != nil || len(ns) != 1 {
		t.Errorf("NamespaceScope = %+v, %v", ns, err)
	}
}

// Under a shape that has a group this adapter's principals name none, so a build
// that reached it forgot its own: every action, core or not, is denied rather
// than allowed without the gates the real authorizer applies.
func TestAuthorizer_UnderAGroupShapeDeniesEverything(t *testing.T) {
	shapetest.NeedsGroup(t)
	for _, act := range []identity.Action{
		identity.ActionCreateNamespace, identity.ActionManageBin, identity.ActionManageCron, identity.ActionManageEtc,
	} {
		err := OpenAuthorizer{}.Authorize(context.Background(), OpenPrincipal{}, act, identity.Resource{})
		if !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s under a group shape: err = %v, want a denial", act, err)
		}
	}
}

// It allows the closed set of actions the core defines, and any action it has
// never heard of is denied: a build without a product surface has no other.
func TestAuthorizer_AllowsTheCoreAndDeniesTheRest(t *testing.T) {
	if len(shape.BuildLayout().Columns) > 0 {
		t.Skip("a shape with a group denies everything; see the test above")
	}
	for _, act := range []identity.Action{
		identity.ActionCreateNamespace, identity.ActionDeleteNamespace, identity.ActionManageNamespaceTokens,
		identity.ActionManageBin, identity.ActionManageEtc, identity.ActionManageCron,
		identity.ActionPublishOrgWide, identity.ActionGrantNamespaceRead,
	} {
		if err := (OpenAuthorizer{}).Authorize(context.Background(), OpenPrincipal{}, act, identity.Resource{}); err != nil {
			t.Errorf("%s was refused: %v", act, err)
		}
	}
	err := OpenAuthorizer{}.Authorize(context.Background(), OpenPrincipal{}, "hosted/manage-billing", identity.Resource{})
	if !errors.Is(err, identity.ErrDenied) {
		t.Errorf("an action of another adapter: err = %v, want a denial", err)
	}
}
