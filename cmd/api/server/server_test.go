package server

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/deployment"
)

// An api with no way to authenticate its callers, to say what they may do or
// to keep what it serves must not start, so an empty deployment is refused with
// a message that names every missing part.
func TestValidate_RequiresIdentityAndBackends(t *testing.T) {
	err := validate(&deployment.Deployment{})
	if err == nil {
		t.Fatal("validate() = nil, want an error naming the missing parts")
	}
	for _, part := range []string{"Access", "Backends.Tokens", "Backends.IngestFailures", "Mounts"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("validate() = %v, want it to name %s", err, part)
		}
	}
	if err := validate(nil); err == nil {
		t.Error("validate(nil) = nil, want an error")
	}
}
