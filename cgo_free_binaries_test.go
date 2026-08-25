package weft_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// cgoFreeBuild matches the packer inline provisioner's CGO-free build
// lines, e.g.
//
//	"cd /tmp/weft && CGO_ENABLED=0 go build -o /tmp/weft-api ./cmd/api",
//
// capturing the package. Deriving the list from the template rather
// than hardcoding it means a binary that later flips to CGO_ENABLED=0
// picks up the guard for free.
var cgoFreeBuild = regexp.MustCompile(`CGO_ENABLED=0 go build [^"]*?(\./cmd/[\w-]+)`)

// TestCGOFreeBinariesLoadWithoutCgo checks that every binary packer
// builds with CGO_ENABLED=0 still has a loadable dependency graph in
// that mode. A cgo-only package anywhere in the graph — DuckDB's
// bindings being the one that reaches us — has all of its files
// excluded by build constraints, and the build dies before compiling
// anything.
//
// `go list -deps` applies build constraints without compiling, so this
// reproduces the failure in about a second and reports the same import
// chain the build does.
//
// 2026-07-30: c51d81f added an `api/notices.go` import of the root
// coordinator package for RenderNotice. That one edge attached
// coordinator → compute → shard → duckdb-go to cmd/api, which packer
// builds cgo-free, and builds 101 and 102 both died four minutes into
// the packer run with "build constraints exclude all Go files in
// .../duckdb-go-bindings/lib/linux-arm64". Nothing in the suite could
// have caught it: every binary compiles fine with cgo on.
func TestCGOFreeBinariesLoadWithoutCgo(t *testing.T) {
	raw, err := os.ReadFile("packer/weft.pkr.hcl")
	if err != nil {
		t.Fatal(err)
	}
	matches := cgoFreeBuild.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no CGO_ENABLED=0 builds found in packer/weft.pkr.hcl — did the build lines move or change shape?")
	}

	for _, m := range matches {
		pkg := m[1]
		t.Run(pkg, func(t *testing.T) {
			// GOOS/GOARCH pinned to the AMI's platform so the answer
			// doesn't depend on the developer's laptop: the constraint
			// that excludes the DuckDB bindings is per-platform.
			cmd := exec.Command("go", "list", "-deps", pkg)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
			// Stdout is the dep list itself — thousands of lines, and
			// noise here. The offending import chain goes to stderr.
			var stderr bytes.Buffer
			cmd.Stdout = io.Discard
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Errorf("%s does not load with CGO_ENABLED=0, so `CGO_ENABLED=0 go build %s` in packer/weft.pkr.hcl will fail the AMI build:\n\n%s\nDrop the cgo-only dependency out of this binary's import graph — usually by moving the symbol it needs into a leaf package — rather than flipping the build to CGO_ENABLED=1.",
					pkg, pkg, stderr.String())
			}
		})
	}
}
