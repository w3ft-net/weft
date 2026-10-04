package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTombstoneBeatsLegacyAcquireLease(t *testing.T) {
	db := testOpen(t)
	const id = 990001
	name := CronLeaseName(id)
	resetLease(t, db, name)

	// Legacy holder has it, then the cron flips to claims.
	if ok, _ := db.AcquireLease(name, "coord-a", LeaseTTL); !ok {
		t.Fatal("setup: coord-a should acquire")
	}
	if err := db.TombstoneCronLease(id); err != nil {
		t.Fatal(err)
	}

	// The old holder's renewal and a newcomer are both denied, forever.
	for _, who := range []string{"coord-a", "coord-b"} {
		if ok, err := db.AcquireLease(name, who, LeaseTTL); err != nil || ok {
			t.Fatalf("%s acquired a tombstoned lease (ok=%v err=%v)", who, ok, err)
		}
	}
	if _, ok, err := db.CronTombstone(id); err != nil || !ok {
		t.Fatalf("CronTombstone ok=%v err=%v", ok, err)
	}
}

func TestTombstoneKeepsOriginalAcquiredAt(t *testing.T) {
	db := testOpen(t)
	const id = 990002
	resetLease(t, db, CronLeaseName(id))

	if err := db.TombstoneCronLease(id); err != nil {
		t.Fatal(err)
	}
	first, _, _ := db.CronTombstone(id)
	if err := db.db.Exec(`UPDATE leases SET acquired_at = acquired_at - interval '1 hour' WHERE name = ?`, CronLeaseName(id)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.TombstoneCronLease(id); err != nil {
		t.Fatal(err)
	}
	second, _, _ := db.CronTombstone(id)
	if !second.Before(first) {
		t.Fatalf("re-tombstone moved acquired_at forward: %v -> %v", first, second)
	}
}

func TestRollbackTombstoneExpires(t *testing.T) {
	db := testOpen(t)
	const id = 990003
	name := CronLeaseName(id)
	resetLease(t, db, name)

	if ok, err := db.RollbackCronTombstone(id); err != nil || ok {
		t.Fatalf("rollback of a non-tombstoned cron: ok=%v err=%v", ok, err)
	}
	if err := db.TombstoneCronLease(id); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RollbackCronTombstone(id); err != nil || !ok {
		t.Fatalf("rollback: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := db.CronTombstone(id); ok {
		t.Fatal("tombstone still present after rollback")
	}

	// Lease path is denied during the 45s window...
	if ok, _ := db.AcquireLease(name, "coord-a", LeaseTTL); ok {
		t.Fatal("lease acquired during rollback window")
	}
	// ...and succeeds once it passes.
	if err := db.db.Exec(`UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = ?`, name).Error; err != nil {
		t.Fatal(err)
	}
	if ok, err := db.AcquireLease(name, "coord-a", LeaseTTL); err != nil || !ok {
		t.Fatalf("lease not acquirable after rollback expiry: ok=%v err=%v", ok, err)
	}
}

func TestClaimCronSlotExactlyOnceUnderConcurrency(t *testing.T) {
	db, err := Open(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	key := fmt.Sprintf("test:%d:slot", time.Now().UnixNano())
	t.Cleanup(func() { db.db.Exec(`DELETE FROM cron_claims WHERE key = ?`, key) })

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := db.ClaimCronSlot(key, fmt.Sprintf("coord-%d", i))
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("want exactly 1 winner, got %d", wins.Load())
	}
}

func TestDeleteCronClaimsBefore(t *testing.T) {
	db := testOpen(t)
	for _, k := range []string{"cleanup:old", "cleanup:new"} {
		if ok, err := db.ClaimCronSlot(k, "me"); err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", k, ok, err)
		}
	}
	if err := db.db.Exec(`UPDATE cron_claims SET claimed_at = now() - interval '2 hours' WHERE key = 'cleanup:old'`).Error; err != nil {
		t.Fatal(err)
	}
	n, err := db.DeleteCronClaimsBefore(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("deleted %d, want >=1", n)
	}
	if ok, _ := db.ClaimCronSlot("cleanup:old", "me"); !ok {
		t.Fatal("old claim should have been removed")
	}
	if ok, _ := db.ClaimCronSlot("cleanup:new", "me"); ok {
		t.Fatal("recent claim should have been kept")
	}
}
