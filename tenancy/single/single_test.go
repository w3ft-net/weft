package single

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"github.com/graybeardcorp/weft/tenancy"
)

// This adapter names no tenant. Installed by mistake under a shape that has
// one (a hosted build that forgot its adapter), its scope must be refused, not
// read as everyone's data.
func TestScoper_UnderATenantShapeTheScopeIsRefused(t *testing.T) {
	shapetest.NeedsTenant(t)
	scope, err := Scoper{}.ReadScope(context.Background(), Actor{})
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
	a := Actor{User: "u"}
	got, err := Scoper{}.ReadScope(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a.Partition().Coord() {
		t.Errorf("ReadScope = %+v, want the one whole-partition tuple", got)
	}
	if _, err := (Scoper{}).ReadScope(context.Background(), nil); !errors.Is(err, tenancy.ErrDenied) {
		t.Errorf("no actor: err = %v, want a denial", err)
	}
	ns, err := Scoper{}.NamespaceScope(context.Background(), shape.Added{}, "any")
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
		err := Authorizer{}.Authorize(context.Background(), Actor{}, act, tenancy.Resource{})
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
		if err := (Authorizer{}).Authorize(context.Background(), Actor{}, act, tenancy.Resource{}); err != nil {
			t.Errorf("%s was refused: %v", act, err)
		}
	}
	err := Authorizer{}.Authorize(context.Background(), Actor{}, "hosted/manage-billing", tenancy.Resource{})
	if !errors.Is(err, tenancy.ErrDenied) {
		t.Errorf("an action of another adapter: err = %v, want a denial", err)
	}
}

// With no organizations every partition is org 0 and every org is the empty
// columns; an actor carries only its user.
func TestOrgs_HaveNoOrganizations(t *testing.T) {
	var o Orgs
	if got := o.OrgOf(shape.Added{}); got != 0 {
		t.Errorf("OrgOf = %d, want 0", got)
	}
	if got := o.AddedOf(7); got != (shape.Added{}) {
		t.Errorf("AddedOf(7) = %+v, want the empty columns", got)
	}
	a := o.ActorOf(7, "u1")
	if a.Subject() != "u1" || a.Partition() != (shape.Added{}) {
		t.Errorf("ActorOf = %+v", a)
	}
}
