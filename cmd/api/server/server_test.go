package server

import (
	"strings"
	"testing"
)

// An api with no way to authenticate its callers, to say what they may do or
// to keep what it serves must not start, so the zero Deps is refused with a
// message that names every missing part.
func TestDeps_RequiresIdentityAndBackends(t *testing.T) {
	err := (&Deps{}).Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error naming the missing parts")
	}
	for _, part := range []string{"Identity.Authn", "Identity.Access", "Backends.Tokens", "Backends.IngestFailures", "Mounts"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("Validate() = %v, want it to name %s", err, part)
		}
	}
	if err := (*Deps)(nil).Validate(); err == nil {
		t.Error("Validate() on nil Deps = nil, want an error")
	}
}
