package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The open api is the cgo-free binary: it loads with CGO_ENABLED=0 and links
// neither a Postgres driver, the hosted store, DuckDB nor the coordinator.
func TestOpenAPI_ImportsNoStoreAndNeedsNoCgo(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list with CGO_ENABLED=0: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"github.com/lib/pq", "github.com/graybeardcorp/weft", "github.com/graybeardcorp/saaskit", "duckdb"} {
			if strings.Contains(dep, banned) {
				t.Errorf("the open api imports %s", dep)
			}
		}
		for _, banned := range []string{"coordinator", "shard", "compute"} {
			if dep == "github.com/w3ft-net/weft/"+banned {
				t.Errorf("the open api imports %s", dep)
			}
		}
	}
}
