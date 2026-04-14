package store

import (
	"os"
	"testing"
)

func testOpen(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	db, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})
	return tx
}

func createTestToken(db *Store, custID uint, label string) (*AgentToken, error) {
	ns, _ := db.FindOrCreateNamespace(custID, "test")
	return db.CreateAgentToken(custID, ns.ID, label)
}

func TestUpdateAgentMetricsConfig(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("metrics-test")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := createTestToken(db, cust.ID, "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	// Update metrics config.
	err = db.UpdateAgentMetricsConfig(tok.Token, true, "15s", []string{"cpu", "memory"})
	if err != nil {
		t.Fatal(err)
	}

	// Reload and verify.
	var updated AgentToken
	db.db.Where("token = ?", tok.Token).First(&updated)

	if !updated.MetricsEnabled {
		t.Error("expected metrics_enabled=true")
	}
	if updated.MetricsInterval != "15s" {
		t.Errorf("expected interval=15s, got %s", updated.MetricsInterval)
	}
	if updated.MetricsCollectors != `["cpu","memory"]` {
		t.Errorf("expected collectors=[cpu,memory], got %s", updated.MetricsCollectors)
	}
}

func TestForecastMetricsUsage(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("forecast-test")
	if err != nil {
		t.Fatal(err)
	}

	// Create two tokens with metrics.
	for _, label := range []string{"agent-1", "agent-2"} {
		tok, err := createTestToken(db, cust.ID, label)
		if err != nil {
			t.Fatal(err)
		}
		err = db.UpdateAgentMetricsConfig(tok.Token, true, "30s", []string{"cpu", "memory", "disk"})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Create one token without metrics (should not count).
	_, err = createTestToken(db, cust.ID, "agent-no-metrics")
	if err != nil {
		t.Fatal(err)
	}

	forecast, err := db.ForecastMetricsUsage(cust.ID)
	if err != nil {
		t.Fatal(err)
	}

	if forecast.AgentsWithMetrics != 2 {
		t.Errorf("expected 2 agents with metrics, got %d", forecast.AgentsWithMetrics)
	}

	// 2 agents × 3 collectors × (86400/30) ticks/day × 300 bytes × 30 days
	// = 2 × 3 × 2880 × 300 × 30 = 155,520,000
	expected := int64(2 * 3 * 2880 * 300 * 30)
	if forecast.ProjectedBytesMonth != expected {
		t.Errorf("expected projected bytes %d, got %d", expected, forecast.ProjectedBytesMonth)
	}
}

func TestForecastMetricsUsage_NoMetrics(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("no-metrics-test")
	if err != nil {
		t.Fatal(err)
	}

	// Token without metrics.
	_, err = createTestToken(db, cust.ID, "plain-agent")
	if err != nil {
		t.Fatal(err)
	}

	forecast, err := db.ForecastMetricsUsage(cust.ID)
	if err != nil {
		t.Fatal(err)
	}

	if forecast.AgentsWithMetrics != 0 {
		t.Errorf("expected 0 agents with metrics, got %d", forecast.AgentsWithMetrics)
	}
	if forecast.ProjectedBytesMonth != 0 {
		t.Errorf("expected 0 projected bytes, got %d", forecast.ProjectedBytesMonth)
	}
}
