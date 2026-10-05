package store

import (
	"context"
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

func TestCronModes(t *testing.T) {
	db := testOpen(t)
	org, err := db.FindOrCreateOrg("cron-modes-test")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, enabled bool) *CronWatch {
		w := &CronWatch{OrgID: org.ID, Name: name, Schedule: "every 5m", Pipeline: "true", Enabled: enabled}
		if err := db.db.WithContext(WithoutTenantScope(context.Background())).Create(w).Error; err != nil {
			t.Fatal(err)
		}
		// Enabled defaults to true in the model; make a disabled one explicit.
		if !enabled {
			db.db.WithContext(WithoutTenantScope(context.Background())).Model(w).Update("enabled", false)
		}
		return w
	}
	plain := mk("m-plain", true)
	held := mk("m-held", true)
	claims := mk("m-claims", true)
	rolled := mk("m-rolled", true)
	disabled := mk("m-disabled", false)

	if ok, _ := db.AcquireLease(CronLeaseName(held.ID), "coord-a", LeaseTTL); !ok {
		t.Fatal("setup: lease")
	}
	if err := db.TombstoneCronLease(claims.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.TombstoneCronLease(rolled.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RollbackCronTombstone(rolled.ID); err != nil || !ok {
		t.Fatalf("setup: rollback ok=%v err=%v", ok, err)
	}
	if err := db.TombstoneCronLease(disabled.ID); err != nil {
		t.Fatal(err)
	}

	modes, err := db.CronModes()
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint]CronMode{}
	for _, m := range modes {
		got[m.WatchID] = m
	}
	if _, present := got[disabled.ID]; present {
		t.Error("disabled cron listed")
	}
	for _, c := range []struct {
		w      *CronWatch
		mode   string
		holder string
	}{
		{plain, CronModeLease, ""},
		{held, CronModeLease, "coord-a"},
		{claims, CronModeClaims, CronClaimsHolder},
		{rolled, CronModeRollback, CronRollbackHolder},
	} {
		m, ok := got[c.w.ID]
		if !ok {
			t.Fatalf("%s missing from CronModes", c.w.Name)
		}
		if m.Mode != c.mode || m.Holder != c.holder {
			t.Errorf("%s: mode=%q holder=%q, want %q/%q", c.w.Name, m.Mode, m.Holder, c.mode, c.holder)
		}
	}
	if !got[claims.ID].ExpiresAt.IsZero() {
		t.Error("tombstone should report no expiry")
	}
	if got[held.ID].ExpiresAt.IsZero() {
		t.Error("a live lease should report its expiry")
	}
}
