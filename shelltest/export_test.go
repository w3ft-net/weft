package shelltest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExportFixtures writes the fixture data as JSON for the frontend
// vitest harness. Run via `go generate` in this package, which names the
// directory in WEFT_FIXTURES_DIR; without it the test writes nothing, so a
// plain `go test` never writes outside this module.
func TestExportFixtures(t *testing.T) {
	outDir := os.Getenv("WEFT_FIXTURES_DIR")
	if outDir == "" {
		t.Skip("WEFT_FIXTURES_DIR not set; run go generate to export")
	}
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
