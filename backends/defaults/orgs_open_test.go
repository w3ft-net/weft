package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// With no organizations a principal carries only its user.
func TestOpenOrgs_HaveNoOrganizations(t *testing.T) {
	var o OpenOrgs
	a := o.PrincipalOf(shapetest.Added(7), "u1")
	if a.Subject() != "u1" || a.Partition() != (shape.Added{}) {
		t.Errorf("PrincipalOf = %+v", a)
	}
}
