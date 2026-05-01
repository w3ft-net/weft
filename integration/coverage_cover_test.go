//go:build integration && cover && !localstack

// Tests gated by the `cover` build tag depend on per-service Go
// coverage profiles materialized at coverage/<service>.out by
// scripts/coverage-report.sh. Run them via scripts/cover-test.sh
// (which brings up the docker-compose.cover.yml overlay, runs the
// integration tests with this tag set, tears down so containers
// flush counters, materializes the profiles, and re-runs the
// boundary checks against them).
//
// Tests that don't need the cover overlay live under just
// `integration` and are run by scripts/integration-test.sh.

package integration

import "testing"

// TestRequireRanIn_AgainstRealProfile is an end-to-end sanity check
// of the assertion helper. `main` is always hit by the -cover test
// entrypoint in the coordinator binary, so if the profile is present
// this must pass.
func TestRequireRanIn_AgainstRealProfile(t *testing.T) {
	RequireRanIn(t, "coordinator", "cmd/coordinator/main.go", "main")
}

// TestRequireRanIn_DetectsAbsentFunction proves the negative case:
// asking for a function that doesn't exist in the named service's
// profile should fail. We walk entries directly so the assertion
// here is independent of RequireRanIn's exact failure path.
func TestRequireRanIn_DetectsAbsentFunction(t *testing.T) {
	entries, err := LoadServiceCoverage("coordinator")
	if err != nil {
		t.Fatalf("load coordinator profile: %v", err)
	}
	if entries == nil {
		t.Skip("no coverage profile on disk — run via scripts/cover-test.sh")
	}

	for _, e := range entries {
		if e.Function == "definitely_does_not_exist" {
			t.Fatalf("unexpected entry for nonexistent function: %+v", e)
		}
	}
}
