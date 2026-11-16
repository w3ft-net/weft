package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func depsOf(t *testing.T, cgo string, tags ...string) []string {
	t.Helper()
	args := []string{"list", "-deps"}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list with CGO_ENABLED=%s tags=%v: %v\n%s", cgo, tags, err, out)
	}
	return strings.Fields(string(out))
}

func linksDuckDB(deps []string) bool {
	for _, d := range deps {
		if strings.Contains(d, "duckdb") {
			return true
		}
	}
	return false
}

// The default compute reads archives through the Lambda and so is the cgo-free
// binary: it loads with CGO_ENABLED=0 and links no DuckDB.
func TestCompute_DefaultBuildLinksNoDuckDB(t *testing.T) {
	if linksDuckDB(depsOf(t, "0")) {
		t.Error("the default weft-compute links DuckDB")
	}
}

// The localreader tag is what links DuckDB in, for reading archives in-process.
func TestCompute_LocalReaderTagLinksDuckDB(t *testing.T) {
	if !linksDuckDB(depsOf(t, "1", "localreader")) {
		t.Error("weft-compute built with -tags localreader does not link DuckDB")
	}
}
