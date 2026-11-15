package server

import (
	"strings"
	"testing"
)

// An api with no way to authenticate its callers or to say what they may do
// must not start, so the zero adapter is refused with a message that says why.
func TestAdapter_RequiresIdentityAndTenancy(t *testing.T) {
	err := Adapter[struct{}]{}.validate()
	if err == nil || !strings.Contains(err.Error(), "Identity") && !strings.Contains(err.Error(), "Tenancy") && !strings.Contains(err.Error(), "Open") {
		t.Fatalf("validate() = %v, want an error naming the missing part", err)
	}
}
