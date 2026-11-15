package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// With no organizations an actor carries only its user.
func TestOpenOrgs_HaveNoOrganizations(t *testing.T) {
	var o OpenOrgs
	a := o.ActorOf(shapetest.Added(7), "u1")
	if a.Subject() != "u1" || a.Partition() != (shape.Added{}) {
		t.Errorf("ActorOf = %+v", a)
	}
}
