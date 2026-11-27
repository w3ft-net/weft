package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The open coordinator has no hosted store: no Postgres driver and nothing of
// the hosted tree.
func TestOpenCoordinator_ImportsNoStore(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"github.com/lib/pq", "github.com/graybeardcorp/weft", "github.com/graybeardcorp/saaskit"} {
			if strings.Contains(dep, banned) {
				t.Errorf("the open coordinator imports %s", dep)
			}
		}
	}
}
