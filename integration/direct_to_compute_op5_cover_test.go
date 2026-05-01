//go:build integration && cover && !localstack

// Coverage-boundary check for direct-to-compute op 5. Lives separate
// from the main op 5 test (direct_to_compute_op5_test.go) because it
// depends on coverage/coordinator.out being on disk — produced by
// scripts/cover-test.sh after a coverage-instrumented run.

package integration

import "testing"

// TestDirectToCompute_Op5_BoundaryCoverage pins op 5's new code path:
// the coordinator's direct-to-compute branch must be what ran. Checks
// that `executeQuery` has positive coverage in the coordinator
// profile — if op 5 regressed and we took some other path, this
// breaks the expected coverage footprint.
func TestDirectToCompute_Op5_BoundaryCoverage(t *testing.T) {
	RequireRanIn(t, "coordinator", "coordinator/coordinator.go", "executeQuery")
}
