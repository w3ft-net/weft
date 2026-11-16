package deployment

import (
	"strings"
	"testing"
)

// A deployment that lacks a part every process needs is refused with a message
// that names every missing part, not the first.
func TestValidate_NamesEveryMissingPart(t *testing.T) {
	err := (&Deployment{}).Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	for _, part := range []string{"Access", "Backends.Tokens", "Backends.Sources", "Backends.Crons", "Backends.Bins", "Backends.IngestFailures", "Mounts"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("Validate() = %v, want it to name %s", err, part)
		}
	}
	if err := (*Deployment)(nil).Validate(); err == nil {
		t.Error("Validate() on a nil deployment = nil, want an error")
	}
}
