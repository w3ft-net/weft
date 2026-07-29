package store

import (
	"testing"
	"time"
)

// TestRecordAgentVersion exercises the full round-trip: stamp a token's
// last_seen_version/at, then read it back via AgentsAtOrBelow. This is
// the wire-protocol gate's evidence — without these two methods working,
// the operator can't tell when an old wire-shape branch is safe to
// remove.
func TestRecordAgentVersion(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("agent-version-test")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := createTestToken(db, cust.ID, "agent-versioning")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := db.crossTenant().Model(&AgentToken{}).Where("id = ?", tok.ID).
		Updates(map[string]any{
			"last_seen_version": "weftd/0.14.0+abc123",
			"last_seen_at":      &now,
		}).Error; err != nil {
		t.Fatalf("record agent version: %v", err)
	}

	// AgentsAtOrBelow returns this token when the version compares
	// lexically <= the threshold.
	got, err := db.AgentsAtOrBelow("weftd/0.14.99", time.Hour)
	if err != nil {
		t.Fatalf("AgentsAtOrBelow: %v", err)
	}
	found := false
	for _, s := range got {
		if s.TokenID == tok.ID {
			found = true
			if s.LastSeenVersion != "weftd/0.14.0+abc123" {
				t.Errorf("LastSeenVersion = %q, want %q", s.LastSeenVersion, "weftd/0.14.0+abc123")
			}
			if s.LastSeenAt == nil || time.Since(*s.LastSeenAt) > time.Minute {
				t.Errorf("LastSeenAt missing or stale: %v", s.LastSeenAt)
			}
		}
	}
	if !found {
		t.Errorf("agent not in AgentsAtOrBelow(0.14.99) list; got %d sightings", len(got))
	}

	// A token version above the threshold must not appear.
	got, err = db.AgentsAtOrBelow("weftd/0.13.99", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.TokenID == tok.ID {
			t.Errorf("token at 0.14.0 incorrectly returned for AtOrBelow(0.13.99)")
		}
	}
}

// TestAgentsAtOrBelow_LookbackWindow asserts that a token whose
// last_seen_at is older than the lookback is excluded. Without this the
// deprecation gate would refuse to drop old-shape code as long as the
// stale row sat in the table.
func TestAgentsAtOrBelow_LookbackWindow(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("lookback-test")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := createTestToken(db, cust.ID, "lookback")
	if err != nil {
		t.Fatal(err)
	}

	// Backdate the row to two days ago by writing the columns directly.
	twoDaysAgo := time.Now().Add(-48 * time.Hour)
	err = db.crossTenant().Model(&AgentToken{}).
		Where("id = ?", tok.ID).
		Updates(map[string]any{
			"last_seen_version": "weftd/0.14.0",
			"last_seen_at":      &twoDaysAgo,
		}).Error
	if err != nil {
		t.Fatal(err)
	}

	// 1h lookback: must NOT include the backdated row.
	got, err := db.AgentsAtOrBelow("weftd/0.99.0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.TokenID == tok.ID {
			t.Errorf("backdated token returned within 1h lookback")
		}
	}

	// 72h lookback: must include the backdated row.
	got, err = db.AgentsAtOrBelow("weftd/0.99.0", 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range got {
		if s.TokenID == tok.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("backdated token missing from 72h lookback")
	}
}
