package main

import (
	"testing"

	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/internal/release"
)

// The API refuses extract-form schemas for sources whose agents are older
// than ingest.MinAgentVersionForExtract, because that is the first weftd that
// can read them. So this weftd, which contains the parser that can, must not
// be older than that: otherwise the release that carries the parser would
// itself be refused.
func TestVersionIsAtLeastTheMinimumForExtractSchemas(t *testing.T) {
	if release.VersionNewer(ingest.MinAgentVersionForExtract, Version) {
		t.Fatalf("weftd Version %s is older than ingest.MinAgentVersionForExtract %s: bump cmd/weftd/version.go",
			Version, ingest.MinAgentVersionForExtract)
	}
}

// Same guard for duration_unit: the API refuses a schema using it for any
// source whose agents are older than ingest.MinAgentVersionForDurationUnit, so
// this build must read it.
func TestVersionIsAtLeastTheMinimumForDurationUnit(t *testing.T) {
	if release.VersionNewer(ingest.MinAgentVersionForDurationUnit, Version) {
		t.Fatalf("weftd Version %s is older than ingest.MinAgentVersionForDurationUnit %s: bump cmd/weftd/version.go",
			Version, ingest.MinAgentVersionForDurationUnit)
	}
}
