package store

import (
	"testing"
)

// testOpen connects to the dedicated `weft_test` database on the dev
// postgres server and returns a transaction-wrapped Store that rolls
// back on test cleanup. The test DB is auto-provisioned the first
// time tests run so contributors don't have to remember a manual
// createdb step.
//
// We don't share the `weft` database with the running dev/integration
// stack: a coordinator+shards mid-write at :5433 contends with
// BeginTest's tx for connections and isolation, and a unit test that
// happens to land during heavy stack churn can see CreateUser fail
// with a duplicate-email surprise (a previously-rolled-back insert
// re-replayed via a connection-pool oddity). Pointing tests at their
// own database removes the contention class entirely.
func testOpen(t *testing.T) *Store {
	t.Helper()
	dsn := TestDSN(t)
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

func createTestToken(db *Store, custID uint, label string) (string, *AgentToken, error) {
	ns, _ := db.FindOrCreateNamespace(custID, "test")
	return db.CreateAgentToken(custID, ns.ID, label, "")
}

func TestUpdateAgentMetricsConfig(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("metrics-test")
	if err != nil {
		t.Fatal(err)
	}

	raw, tok, err := createTestToken(db, cust.ID, "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	// Update metrics config.
	err = db.UpdateAgentMetricsConfig(raw, true, "15s", []string{"cpu", "memory"})
	if err != nil {
		t.Fatal(err)
	}

	// Reload and verify.
	var updated AgentToken
	db.crossTenant().Where("token_hash = ?", tok.TokenHash).First(&updated)

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
		raw, _, err := createTestToken(db, cust.ID, label)
		if err != nil {
			t.Fatal(err)
		}
		err = db.UpdateAgentMetricsConfig(raw, true, "30s", []string{"cpu", "memory", "disk"})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Create one token without metrics (should not count).
	_, _, err = createTestToken(db, cust.ID, "agent-no-metrics")
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

// TestAgentToken_HashedAtRest pins the audit-§1 invariant: the raw
// wft_<...> token must not appear in agent_tokens. Only the hash and
// the public prefix live in Postgres.
func TestAgentToken_HashedAtRest(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("agent-hash-test")
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := createTestToken(db, cust.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}

	if raw == "" {
		t.Fatal("empty raw token")
	}
	var row struct {
		TokenHash string
		Prefix    string
	}
	if err := db.db.Table("agent_tokens").
		Select("token_hash, prefix").
		Where("id = ?", tok.ID).
		Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.TokenHash == raw {
		t.Fatalf("raw token stored in db: %q", row.TokenHash)
	}
	if row.TokenHash != hashAgentToken(raw) {
		t.Fatalf("stored hash %q != sha256(raw) %q", row.TokenHash, hashAgentToken(raw))
	}
	if !stringsHasPrefix(raw, row.Prefix) {
		t.Fatalf("prefix %q is not a prefix of raw %q", row.Prefix, raw)
	}

	// Validate round-trips: presenting the raw token hashes in-flight
	// and finds the row.
	res, err := db.ValidateAgentToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("validate: expected hit, got nil")
	}

	// Random strings don't validate.
	res, err = db.ValidateAgentToken("wft_not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatal("validate: expected miss on bogus token")
	}
}

// TestAgentToken_RevokeByID confirms revocation is ID-based and that
// a revoked token no longer validates.
func TestAgentToken_RevokeByID(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("agent-revoke-test")
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := createTestToken(db, cust.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}

	if err := revokeAgentToken(db, tok.ID); err != nil {
		t.Fatal(err)
	}
	res, err := db.ValidateAgentToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatal("expected validate miss after revoke")
	}
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestForecastMetricsUsage_NoMetrics(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("no-metrics-test")
	if err != nil {
		t.Fatal(err)
	}

	// Token without metrics.
	_, _, err = createTestToken(db, cust.ID, "plain-agent")
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

// revokeAgentToken mirrors the former Store.RevokeAgentToken, whose
// production logic moved to a caller outside the store package.
func revokeAgentToken(db *Store, id uint) error {
	return db.crossTenant().Model(&AgentToken{}).Where("id = ?", id).Update("revoked", true).Error
}
