package store

import (
	"errors"
	"testing"
	"time"
)

// Tests for the batched last_seen_* stamp tracker (docs/plans/
// weftd-offset-checkpointing.md, "Stream-open cost"). Stream opens record
// into memory; a flusher writes one batched statement per interval.

func seenRow(t *testing.T, db *Store, id uint) (string, *time.Time) {
	t.Helper()
	var tok AgentToken
	if err := db.crossTenant().First(&tok, id).Error; err != nil {
		t.Fatal(err)
	}
	return tok.LastSeenVersion, tok.LastSeenAt
}

func newTestTracker(db *Store) (*AgentSeenTracker, *int) {
	tr := NewAgentSeenTracker(db)
	execs := new(int)
	real := tr.exec
	tr.exec = func(sql string, args ...any) error {
		*execs++
		return real(sql, args...)
	}
	return tr, execs
}

func TestAgentSeen_CoalescesTouchesIntoOneStatement(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("seen-coalesce")
	_, a, _ := createTestToken(db, cust.ID, "a")
	_, b, _ := createTestToken(db, cust.ID, "b")

	tr, execs := newTestTracker(db)
	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 100; i++ {
		tr.Touch(a.ID, "weftd/0.20.0", now.Add(time.Duration(i)*time.Second))
	}
	tr.Touch(a.ID, "weftd/0.21.0", now.Add(200*time.Second)) // latest wins
	tr.Touch(b.ID, "weftd/0.19.0", now)

	if err := tr.Flush(); err != nil {
		t.Fatal(err)
	}
	if *execs != 1 {
		t.Errorf("flush issued %d statements for 2 tokens, want 1", *execs)
	}
	if v, at := seenRow(t, db, a.ID); v != "weftd/0.21.0" || at == nil {
		t.Errorf("token a = %q %v, want latest version", v, at)
	}
	if v, _ := seenRow(t, db, b.ID); v != "weftd/0.19.0" {
		t.Errorf("token b = %q", v)
	}
}

func TestAgentSeen_SuppressesNoOpWrites(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("seen-noop")
	_, tok, _ := createTestToken(db, cust.ID, "a")

	tr, execs := newTestTracker(db)
	now := time.Now()
	tr.Touch(tok.ID, "weftd/0.20.0", now)
	tr.Flush()
	if *execs != 1 {
		t.Fatalf("first flush execs=%d, want 1", *execs)
	}

	// Same version, 1 minute later: nothing worth writing.
	tr.Touch(tok.ID, "weftd/0.20.0", now.Add(time.Minute))
	tr.Flush()
	if *execs != 1 {
		t.Errorf("unchanged version within the window wrote again (execs=%d)", *execs)
	}

	// Version change always writes.
	tr.Touch(tok.ID, "weftd/0.21.0", now.Add(2*time.Minute))
	tr.Flush()
	if *execs != 2 {
		t.Errorf("version change did not write (execs=%d)", *execs)
	}

	// Same version but past the refresh window: write to keep last_seen_at fresh.
	tr.Touch(tok.ID, "weftd/0.21.0", now.Add(10*time.Minute))
	tr.Flush()
	if *execs != 3 {
		t.Errorf("stale last_seen_at was not refreshed (execs=%d)", *execs)
	}
}

func TestAgentSeen_FailedFlushRetriesWithoutClobberingNewer(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("seen-retry")
	_, tok, _ := createTestToken(db, cust.ID, "a")

	tr := NewAgentSeenTracker(db)
	real := tr.exec
	now := time.Now().Truncate(time.Millisecond)
	tr.Touch(tok.ID, "weftd/0.20.0", now)

	tr.exec = func(string, ...any) error {
		// A newer stamp lands while the flush is in flight and fails.
		tr.Touch(tok.ID, "weftd/0.21.0", now.Add(time.Minute))
		return errors.New("db down")
	}
	if err := tr.Flush(); err == nil {
		t.Fatal("expected flush error")
	}

	tr.exec = real
	if err := tr.Flush(); err != nil {
		t.Fatal(err)
	}
	if v, _ := seenRow(t, db, tok.ID); v != "weftd/0.21.0" {
		t.Errorf("after retry version = %q, want the newer 0.21.0", v)
	}
}

func TestAgentSeen_FailedFlushKeepsOriginalWhenNothingNewer(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("seen-retry2")
	_, tok, _ := createTestToken(db, cust.ID, "a")

	tr := NewAgentSeenTracker(db)
	real := tr.exec
	tr.Touch(tok.ID, "weftd/0.20.0", time.Now())
	tr.exec = func(string, ...any) error { return errors.New("db down") }
	tr.Flush()

	tr.exec = real
	tr.Flush()
	if v, _ := seenRow(t, db, tok.ID); v != "weftd/0.20.0" {
		t.Errorf("stamp lost after a failed flush: %q", v)
	}
}

func TestAgentSeen_StaleWriteDoesNotRegressNewerDBValue(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("seen-stale")
	_, tok, _ := createTestToken(db, cust.ID, "a")

	// Another coordinator already stamped a newer sighting.
	newer := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	if err := db.crossTenant().Model(&AgentToken{}).Where("id = ?", tok.ID).
		Updates(map[string]any{"last_seen_version": "weftd/0.22.0", "last_seen_at": &newer}).Error; err != nil {
		t.Fatal(err)
	}

	tr := NewAgentSeenTracker(db)
	tr.Touch(tok.ID, "weftd/0.20.0", time.Now())
	if err := tr.Flush(); err != nil {
		t.Fatal(err)
	}
	if v, _ := seenRow(t, db, tok.ID); v != "weftd/0.22.0" {
		t.Errorf("older stamp overwrote newer DB value: %q", v)
	}
}

func TestAgentSeen_NilTrackerIsSafe(t *testing.T) {
	var tr *AgentSeenTracker
	tr.Touch(1, "weftd/0.20.0", time.Now()) // must not panic
}
