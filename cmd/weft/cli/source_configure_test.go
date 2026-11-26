package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An empty schema document must be refused, not applied. `weft source
// configure ns/svc < /dev/null` once read zero bytes, parsed them to a blank
// schema, and replaced a working schema with it.
func TestLoadSchemaNonInteractiveRejectsEmptyDocument(t *testing.T) {
	for name, content := range map[string]string{
		"empty":      "",
		"whitespace": "  \n\n",
		"comment":    "# nothing here\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			schema, err := loadSchemaNonInteractive(path)
			if err == nil {
				t.Fatalf("empty schema document accepted: %+v", schema)
			}
			if !strings.Contains(err.Error(), "empty") {
				t.Errorf("error should say the document is empty, got %q", err)
			}
		})
	}
}
