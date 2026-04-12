package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/kevlarcon/weft/store"
	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// TestAuthenticate_NoShardAddr verifies that the Authenticate response
// does not leak internal shard addresses to external agents.
func TestAuthenticate_NoShardAddr(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	s0, err := shard.NewDuckDBShard("shard-0", "", coordinator.DevT0, coordinator.DevT1)
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
	tok, err := store.CreateAgentToken(cust.ID, "test-agent", "testapp")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Authenticate(context.Background(), &pb.AuthenticateRequest{Token: tok.Token})
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

// TestAuthenticate_InvalidToken verifies that bad tokens are rejected.
func TestAuthenticate_InvalidToken(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	s0, err := shard.NewDuckDBShard("shard-0", "", coordinator.DevT0, coordinator.DevT1)
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
	return db, func() {
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
