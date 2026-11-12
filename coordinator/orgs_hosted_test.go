//go:build !defaultshape

package coordinator

import (
	"testing"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/single"
)

// Hosted weft's org handle and the shape's added columns are the same number,
// and the mapper goes both ways, including through an actor.
func TestOrgs_HostedMapsBothWays(t *testing.T) {
	var o Orgs = hosted.Orgs{}
	if got := o.AddedOf(7); got != shapetest.Added(7) {
		t.Errorf("AddedOf(7) = %+v, want the columns of tenant 7", got)
	}
	if got := o.OrgOf(shapetest.Added(7)); got != store.OrgID(7) {
		t.Errorf("OrgOf(tenant 7) = %d, want 7", got)
	}
	a := o.ActorOf(7, "u1")
	if a.Partition() != shapetest.Added(7) || a.Subject() != "u1" {
		t.Errorf("ActorOf(7, u1) = %+v, partition %+v", a, a.Partition())
	}
	if !noTenant(o.AddedOf(0)) {
		t.Error("org 0 names a tenant")
	}
}

// A coordinator built without an adapter acts as hosted weft in this package's
// tests (hosted_adapter_test.go), and as the open adapter otherwise.
func TestOrgs_ADefaultCoordinatorUsesTheDefaultAdapter(t *testing.T) {
	c := &Coordinator{}
	if _, ok := c.Mapper().(hosted.Orgs); !ok {
		t.Errorf("Mapper() = %T, want hosted.Orgs under the hosted test adapter", c.Mapper())
	}
	// the open adapter has no orgs: every partition is org 0, every org the empty columns
	open := single.Orgs{}
	if got := open.OrgOf(shapetest.Added(7)); got != 0 {
		t.Errorf("the open adapter maps a tenant to org %d, want 0", got)
	}
	if got := open.AddedOf(7); got != (shape.Added{}) {
		t.Errorf("the open adapter maps org 7 to %+v, want the empty columns", got)
	}
	var _ tenancy.Actor = open.ActorOf(1, "u")
}
