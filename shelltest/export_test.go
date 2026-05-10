package shelltest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestExportFixtures writes the fixture data as JSON for the frontend
// vitest harness. Run via `go generate` in this package.
func TestExportFixtures(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	outDir := filepath.Join(dir, "..", "frontend", "src", "test")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	data, err := json.MarshalIndent(Fixtures, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	outPath := filepath.Join(outDir, "shell_fixtures.json")
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote %d bytes to %s", len(data), outPath)
}
