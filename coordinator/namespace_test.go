package coordinator

import (
	"strings"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// setupNamespaceCoordinator creates a coordinator with one shard containing
// data from multiple sub-apps within the "weft" namespace:
//
//	weft/coordinator  — 5 log rows
//	weft/shard        — 5 log rows
//	weft/metrics      — 5 metric rows (JSON payloads)
//
// This mirrors production where weftd on each host writes logs as
// <namespace>/<filename> and metrics as <namespace>/metrics.
func setupNamespaceCoordinator(t *testing.T) *Coordinator {
	t.Helper()

	ds, err := shard.NewDuckDBShard("ns-shard-0", "")
	if err != nil {
		t.Fatalf("create shard: %v", err)
	}
	t.Cleanup(func() { ds.Close() })

	base := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)

	var rows []*pb.LogRow

	// weft/coordinator logs
	for i := 0; i < 5; i++ {
		rows = append(rows, &pb.LogRow{
			TsUnixNs: base.Add(time.Duration(i) * time.Minute).UnixNano(),
			App:      "weft/coordinator",
			Raw:      "coordinator: handling query",
			Level:    "INFO",
			Instance: "i-coord-001",
			Customer: "graybeard",
		})
	}

	// weft/shard logs
	for i := 0; i < 5; i++ {
		rows = append(rows, &pb.LogRow{
			TsUnixNs: base.Add(time.Duration(i) * time.Minute).UnixNano(),
			App:      "weft/shard",
			Raw:      "shard: ingesting batch",
			Level:    "INFO",
			Instance: "i-shard-001",
			Customer: "graybeard",
		})
	}

	// weft/metrics rows (JSON payloads, like system metrics from weftd)
	for i := 0; i < 5; i++ {
		rows = append(rows, &pb.LogRow{
			TsUnixNs:    base.Add(time.Duration(i) * time.Minute).UnixNano(),
			App:         "weft/metrics",
			Raw:         `{"collector":"cpu","value":42}`,
			Level:       "METRIC",
			Instance:    "i-coord-001",
			Customer:    "graybeard",
			PayloadJson: payload(map[string]any{"collector": "cpu", "value": 42}),
		})
	}

	// A row from a different namespace to verify isolation.
	rows = append(rows, &pb.LogRow{
		TsUnixNs: base.UnixNano(),
		App:      "grantcentral",
		Raw:      "grantcentral: should not appear",
		Level:    "INFO",
		Instance: "i-gc-001",
		Customer: "kevlar",
	})

	if err := ds.Ingest(rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	c := NewCoordinator(nil, nil)
	c.Registry.Add(&ShardEntry{
		ID:          "ns-shard-0",
		Apps:        []string{"weft/coordinator", "weft/shard", "weft/metrics", "grantcentral"},
		WindowStart: base,
		WindowEnd:   base.Add(time.Hour),
		State:       StateIngesting,
		Shard:       ds,
		CreatedAt:   time.Now(),
	})
	return c
}

var nsRange = TimeRange{
	Start: time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC),
	End:   time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC),
}

// TestNamespaceQuery_All verifies that querying the namespace pseudofile
// (cat /log/weft.log) returns rows from ALL sub-apps.
func TestNamespaceQuery_All(t *testing.T) {
	c := setupNamespaceCoordinator(t)

	result, err := c.Query("cat /log/weft.log", nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	// Should get all 15 rows (5 coordinator + 5 shard + 5 metrics)
	if len(result.Rows) != 15 {
		t.Errorf("expected 15 rows for namespace query, got %d", len(result.Rows))
	}

	// Verify all three sub-apps are represented by checking raw content.
	appCounts := map[string]int{}
	for _, r := range result.Rows {
		switch {
		case strings.Contains(r.Raw, "coordinator:"):
			appCounts["weft/coordinator"]++
		case strings.Contains(r.Raw, "shard:"):
			appCounts["weft/shard"]++
		case strings.Contains(r.Raw, "collector"):
			appCounts["weft/metrics"]++
		case strings.Contains(r.Raw, "grantcentral"):
			appCounts["grantcentral"]++
		}
	}
	for _, want := range []string{"weft/coordinator", "weft/shard", "weft/metrics"} {
		if appCounts[want] != 5 {
			t.Errorf("expected 5 rows for %s, got %d", want, appCounts[want])
		}
	}

	// grantcentral must NOT appear (different namespace, different customer).
	if appCounts["grantcentral"] > 0 {
		t.Errorf("grantcentral rows leaked into weft namespace query")
	}
}

// TestNamespaceQuery_SubApp verifies that querying a specific sub-app
// (cat /log/weft/coordinator.log) returns only that sub-app's rows.
func TestNamespaceQuery_SubApp(t *testing.T) {
	c := setupNamespaceCoordinator(t)

	result, err := c.Query("cat /log/weft/coordinator.log", nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if len(result.Rows) != 5 {
		t.Errorf("expected 5 rows for sub-app query, got %d", len(result.Rows))
	}
	for _, r := range result.Rows {
		if !strings.Contains(r.Raw, "coordinator:") {
			t.Errorf("unexpected raw in sub-app query: %q", r.Raw)
		}
	}
}

// TestNamespaceQuery_Metrics verifies that metrics sub-app is queryable
// via the /namespace/metrics path (not the old .metrics convention).
func TestNamespaceQuery_Metrics(t *testing.T) {
	c := setupNamespaceCoordinator(t)

	result, err := c.Query("cat /log/weft/metrics.log", nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if len(result.Rows) != 5 {
		t.Errorf("expected 5 rows for metrics query, got %d", len(result.Rows))
	}
	for _, r := range result.Rows {
		if !strings.Contains(r.Raw, "collector") {
			t.Errorf("unexpected raw in metrics query: %q", r.Raw)
		}
	}
}

// TestNamespaceQuery_HeadTail verifies that head/tail work with namespace queries.
func TestNamespaceQuery_HeadTail(t *testing.T) {
	c := setupNamespaceCoordinator(t)

	result, err := c.Query("head -3 /log/weft.log", nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("head query: %v", err)
	}
	if len(result.Rows) != 3 {
		t.Errorf("expected 3 rows for head -3, got %d", len(result.Rows))
	}

	result, err = c.Query("tail -3 /log/weft.log", nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("tail query: %v", err)
	}
	if len(result.Rows) != 3 {
		t.Errorf("expected 3 rows for tail -3, got %d", len(result.Rows))
	}
}

// TestNamespaceQuery_FilterAcrossSubApps verifies that grep/awk filters
// work correctly across sub-apps in a namespace query.
func TestNamespaceQuery_FilterAcrossSubApps(t *testing.T) {
	c := setupNamespaceCoordinator(t)

	// grep for "coordinator" — should only match weft/coordinator rows
	result, err := c.Query(`cat /log/weft.log | grep coordinator`, nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Rows) != 5 {
		t.Errorf("expected 5 rows matching 'coordinator', got %d", len(result.Rows))
	}

	// awk filter on instance — should match only shard rows
	result, err = c.Query(`cat /log/weft.log | awk '$INSTANCE == "i-shard-001"'`, nsRange, "graybeard", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Rows) != 5 {
		t.Errorf("expected 5 rows from i-shard-001, got %d", len(result.Rows))
	}
}

// TestSeenApps_TracksSubApps verifies that SeenApps collects distinct
// sub-app names as data flows through the coordinator.
func TestSeenApps_TracksSubApps(t *testing.T) {
	sa := NewSeenApps()

	sa.Add("graybeard", "weft/coordinator", "")
	sa.Add("graybeard", "weft/shard", "")
	sa.Add("graybeard", "weft/metrics", "")
	sa.Add("graybeard", "weft/coordinator", "") // duplicate
	sa.Add("kevlar", "grantcentral", "")

	// Per-customer filtering.
	names := sa.Names("graybeard")
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
	kevlarNames := sa.Names("kevlar")
	if len(kevlarNames) != 1 || kevlarNames[0] != "grantcentral" {
		t.Errorf("kevlar apps: got %v, want [grantcentral]", kevlarNames)
	}

	// Unknown customer sees nothing.
	if names := sa.Names("unknown"); len(names) != 0 {
		t.Errorf("unknown customer should see no apps, got %v", names)
	}
}

// TestSeenApps_EnvEncoding verifies that env-bearing rows produce
// the slash-encoded "<ns>/<env>/<app>" name shape that drives the
// 3-level pseudofile path layout. See docs/plans/ingest-dimensions.md.
func TestSeenApps_EnvEncoding(t *testing.T) {
	sa := NewSeenApps()

	// Mix of env-less and env-bearing entries in the same namespace.
	sa.Add("acme", "myco/api", "")          // env-less
	sa.Add("acme", "myco/api", "prod")      // env-bearing, same app
	sa.Add("acme", "myco/api", "staging")   // different env, same app
	sa.Add("acme", "myco/worker", "prod")   // different app under prod

	names := sa.Names("acme")
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
