package weft_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAllCmdPackagesBuild verifies that every cmd/* package compiles.
// This catches typos, wrong variable references, and missing imports
// that go vet alone won't find — the kind of errors that only surface
// during docker builds.
func TestAllCmdPackagesBuild(t *testing.T) {
	entries, err := os.ReadDir("cmd")
	if err != nil {
		t.Fatalf("reading cmd/: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()

		// cmd/wasm requires GOOS=js GOARCH=wasm — test separately.
		if name == "wasm" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pkg := "./cmd/" + name
			cmd := exec.Command("go", "build", "-o", os.DevNull, pkg)
			cmd.Dir = "."
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go build %s failed:\n%s", pkg, strings.TrimSpace(string(out)))
			}
		})
	}
}

func TestWasmBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM build in short mode")
	}
	cmd := exec.Command("go", "build", "-o", os.DevNull, "./cmd/wasm")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build ./cmd/wasm (GOOS=js) failed:\n%s", strings.TrimSpace(string(out)))
	}
}
