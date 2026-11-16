package coordinator

import (
	"testing"

	"github.com/w3ft-net/weft/shape/shapetest"
)

// TestSeenServices_TracksSubApps verifies that SeenServices collects distinct
// sub-app names as data flows through the coordinator.
func TestSeenServices_TracksSubApps(t *testing.T) {
	shapetest.NeedsGroup(t)
	sa := NewSeenServices()

	sa.Add(shapetest.Added(9), "weft/coordinator", "")
	sa.Add(shapetest.Added(9), "weft/shard", "")
	sa.Add(shapetest.Added(9), "weft/metrics", "")
	sa.Add(shapetest.Added(9), "weft/coordinator", "") // duplicate
	sa.Add(shapetest.Added(1), "grantcentral", "")

	// Per-customer filtering.
	names := sa.Names(shapetest.Added(9))
	if len(names) != 3 {
		t.Fatalf("expected 3 apps for graybeard, got %d: %v", len(names), names)
	}
	want := []string{"weft/coordinator", "weft/metrics", "weft/shard"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], w)
		}
	}

	// kevlar should only see grantcentral.
	kevlarNames := sa.Names(shapetest.Added(1))
	if len(kevlarNames) != 1 || kevlarNames[0] != "grantcentral" {
		t.Errorf("kevlar apps: got %v, want [grantcentral]", kevlarNames)
	}

	// Unknown customer sees nothing.
	if names := sa.Names(shapetest.Added(999)); len(names) != 0 {
		t.Errorf("unknown customer should see no apps, got %v", names)
	}
}

// TestSeenServices_EnvEncoding verifies that env-bearing rows produce
// the slash-encoded "<ns>/<env>/<app>" name shape that drives the
// 3-level pseudofile path layout.
func TestSeenServices_EnvEncoding(t *testing.T) {
	sa := NewSeenServices()

	// Mix of env-less and env-bearing entries in the same namespace.
	sa.Add(shapetest.Added(5), "myco/api", "")        // env-less
	sa.Add(shapetest.Added(5), "myco/api", "prod")    // env-bearing, same app
	sa.Add(shapetest.Added(5), "myco/api", "staging") // different env, same app
	sa.Add(shapetest.Added(5), "myco/worker", "prod") // different app under prod

	names := sa.Names(shapetest.Added(5))
	want := []string{
		"myco/api",
		"myco/prod/api",
		"myco/prod/worker",
		"myco/staging/api",
	}
	if len(names) != len(want) {
		t.Fatalf("got %d names, want %d: %v", len(names), len(want), names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], w)
		}
	}
}
