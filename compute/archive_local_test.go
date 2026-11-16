package compute

import "testing"

// Where DuckDB is linked (these tests link it) the in-process reader is
// available by name, so a build with DuckDB reads archives with no Lambda.
func TestNewArchiveRouter_LocalReaderWhereDuckDBIsLinked(t *testing.T) {
	if _, err := NewArchiveRouter(map[string]string{"default": ArchiveReaderLocal}, nil, "us-east-2"); err != nil {
		t.Fatalf("the local reader is not registered: %v", err)
	}
}
