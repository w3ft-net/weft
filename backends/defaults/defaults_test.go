package defaults

import (
	"os"
	"os/exec"
	"testing"
)

// The open build links these stores into processes that build with
// CGO_ENABLED=0, so nothing the package imports may need cgo.
func TestDefaultsLoadWithoutCgo(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go list -deps with CGO_ENABLED=0: %v\n%s", err, out)
	}
}
