package main

import (
	"testing"

	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/shard/duckbind"
)

// The Lambda reader is always there; the in-process reader is there where DuckDB
// is linked (these tests link it), so such a build reads archives with no Lambda.
func TestArchiveReaderSet(t *testing.T) {
	set := archiveReaderSet(nil, "us-east-2")
	if set[compute.ArchiveReaderLambda] == nil {
		t.Error("no Lambda reader")
	}
	if duckbind.Linked() != (set[compute.ArchiveReaderLocal] != nil) {
		t.Errorf("local reader present = %v, DuckDB linked = %v", set[compute.ArchiveReaderLocal] != nil, duckbind.Linked())
	}
}
