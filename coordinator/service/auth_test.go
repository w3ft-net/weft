package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestAuthenticate_NoShardAddr verifies that the Authenticate response
// does not leak internal shard addresses to external agents.
func TestAuthenticate_NoShardAddr(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()

	coord := coordinator.NewCoordinator([]shard.Shard{s0}, "kevlar", nil)
	srv := &Server{Coord: coord, Store: store}

	// Create a token.
	cust, err := store.FindOrCreateCustomer("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	store.SeedDevSubscription(cust.Name)
	ns, _ := store.FindOrCreateNamespace(cust.ID, "testapp")
	rawTok, _, err := store.CreateAgentToken(cust.ID, ns.ID, "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Authenticate(context.Background(), &pb.AuthenticateRequest{Token: rawTok})
	if err != nil {
		t.Fatal(err)
	}

	if resp.Customer != "kevlar" {
		t.Errorf("expected customer=kevlar, got %q", resp.Customer)
	}

	// Marshal the response to JSON and check no address-like fields exist.
	// This catches any future additions that might leak infrastructure info.
	data, _ := json.Marshal(resp)
	raw := string(data)
	for _, banned := range []string{"shard_addr", "shardAddr", "172.", "10.", "192.168.", ":9000"} {
		if contains(raw, banned) {
			t.Errorf("AuthenticateResponse contains %q — must not leak shard addresses.\nFull response: %s", banned, raw)
		}
	}
}

// TestAuthenticate_MetricsConfigSync verifies that the agent's reported
// metrics config is persisted to the token record on authentication.
func TestAuthenticate_MetricsConfigSync(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()

	coord := coordinator.NewCoordinator([]shard.Shard{s0}, "kevlar", nil)
	srv := &Server{Coord: coord, Store: store}

	cust, err := store.FindOrCreateCustomer("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	store.SeedDevSubscription(cust.Name)
	ns2, _ := store.FindOrCreateNamespace(cust.ID, "api")
	rawTok, _, err := store.CreateAgentToken(cust.ID, ns2.ID, "metrics-sync-test")
	if err != nil {
		t.Fatal(err)
	}

	// Authenticate with metrics config.
	_, err = srv.Authenticate(context.Background(), &pb.AuthenticateRequest{
		Token: rawTok,
		MetricsConfig: &pb.MetricsConfig{
			Enabled:    true,
			Interval:   "15s",
			Collectors: []string{"cpu", "memory", "disk"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify metrics config was persisted.
	forecast, err := store.ForecastMetricsUsage(cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if forecast.AgentsWithMetrics < 1 {
		t.Error("expected at least 1 agent with metrics after auth")
	}
	if forecast.ProjectedBytesMonth == 0 {
		t.Error("expected non-zero projected bytes")
	}
}

// TestAuthenticate_InvalidToken verifies that bad tokens are rejected.
func TestAuthenticate_InvalidToken(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()

	coord := coordinator.NewCoordinator([]shard.Shard{s0}, "kevlar", nil)
	srv := &Server{Coord: coord, Store: store}

	_, err = srv.Authenticate(context.Background(), &pb.AuthenticateRequest{Token: "wft_bogus"})
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func testStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	return tx, func() {
		rollback()
		db.Close()
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
