package defaults

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/tenancy"
)

// This adapter names no tenant. Installed by mistake under a shape that has
// one (a hosted build that forgot its adapter), its scope must be refused, not
// read as everyone's data.
func TestScoper_UnderATenantShapeTheScopeIsRefused(t *testing.T) {
	shapetest.NeedsTenant(t)
	scope, err := OpenScoper{}.ReadScope(context.Background(), OpenActor{})
	if err != nil {
		return // refused by the scoper itself: also closed
	}
	if _, err := planner.Plan(`cat /log/ns/svc/main.log`, scope, "", time.Time{}, time.Time{}); err == nil {
		t.Fatal("a scope that names no tenant planned under a shape with one")
	}
}

// The adapter of a build with one tenant reads the whole of the actor's
// partition and nothing else.
func TestScoper_ReadsTheActorsWholePartition(t *testing.T) {
	a := OpenActor{User: "u"}
	got, err := OpenScoper{}.ReadScope(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a.Partition().Coord() {
		t.Errorf("ReadScope = %+v, want the one whole-partition tuple", got)
	}
	if _, err := (OpenScoper{}).ReadScope(context.Background(), nil); !errors.Is(err, tenancy.ErrDenied) {
		t.Errorf("no actor: err = %v, want a denial", err)
	}
	ns, err := OpenScoper{}.NamespaceScope(context.Background(), shape.Added{}, "any")
	if err != nil || len(ns) != 1 {
		t.Errorf("NamespaceScope = %+v, %v", ns, err)
	}
}

// Under a shape that has a tenant this adapter's actors name none, so a build
// that reached it forgot its own: every action, core or not, is denied rather
// than allowed without the gates the real authorizer applies.
func TestAuthorizer_UnderATenantShapeDeniesEverything(t *testing.T) {
	shapetest.NeedsTenant(t)
	for _, act := range []tenancy.Action{
		tenancy.ActionCreateNamespace, tenancy.ActionManageBin, tenancy.ActionManageCron, tenancy.ActionManageEtc,
	} {
		err := OpenAuthorizer{}.Authorize(context.Background(), OpenActor{}, act, tenancy.Resource{})
		if !errors.Is(err, tenancy.ErrDenied) {
			t.Errorf("%s under a tenant shape: err = %v, want a denial", act, err)
		}
	}
}

// It allows the closed set of actions the core defines, and any action it has
// never heard of is denied: a build without a product surface has no other.
func TestAuthorizer_AllowsTheCoreAndDeniesTheRest(t *testing.T) {
	if len(shape.BuildLayout().Columns) > 0 {
		t.Skip("a shape with a tenant denies everything; see the test above")
	}
	for _, act := range []tenancy.Action{
		tenancy.ActionCreateNamespace, tenancy.ActionDeleteNamespace, tenancy.ActionManageNamespaceTokens,
		tenancy.ActionManageBin, tenancy.ActionManageEtc, tenancy.ActionManageCron,
		tenancy.ActionPublishOrgWide, tenancy.ActionGrantNamespaceRead,
	} {
		if err := (OpenAuthorizer{}).Authorize(context.Background(), OpenActor{}, act, tenancy.Resource{}); err != nil {
			t.Errorf("%s was refused: %v", act, err)
		}
	}
	err := OpenAuthorizer{}.Authorize(context.Background(), OpenActor{}, "hosted/manage-billing", tenancy.Resource{})
	if !errors.Is(err, tenancy.ErrDenied) {
		t.Errorf("an action of another adapter: err = %v, want a denial", err)
	}
}
