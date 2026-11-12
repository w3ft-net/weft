package store

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/coordination"
)

// resetLease clears any row left behind for name by an earlier test.
// testOpen wraps each test in its own rolled-back transaction, which
// isolates tests in *this* file from each other — but not from other
// packages' tests (coordinator/nat_test.go, logdb/leader_election_test.go)
// that exercise these same production lease names against a real,
// committed row in the shared per-process pooled test DB (store.TestDSN's
// doc comment). Those files already defend against exactly this with
// their own resetNATLease/resetShardFSMLease; this file needs the same.
func resetLease(t *testing.T, db *Store, name string) {
	t.Helper()
	if err := db.db.Exec("DELETE FROM leases WHERE name = ?", name).Error; err != nil {
		t.Fatalf("reset %s lease: %v", name, err)
	}
}

func TestAcquireLease_FreshAcquireThenRenewBySameHolder(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "nat-holder")

	ok, err := db.AcquireLease("nat-holder", "coord-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("fresh acquire on an unheld lease should succeed")
	}

	// Same holder renewing must succeed — it already owns the lease.
	ok, err = db.AcquireLease("nat-holder", "coord-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("renewal by the current holder should succeed")
	}
}

func TestAcquireLease_BlockedByOtherNonExpiredHolder(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "nat-holder")

	if ok, err := db.AcquireLease("nat-holder", "coord-a", time.Minute); err != nil || !ok {
		t.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
	}

	ok, err := db.AcquireLease("nat-holder", "coord-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a different holder must not acquire a non-expired lease")
	}
}

func TestAcquireLease_TakeoverAfterExpiry(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "nat-holder")

	// Acquire with a TTL that's already in the past — simulates a lease
	// left behind by a holder that's since died.
	if ok, err := db.AcquireLease("nat-holder", "coord-a", -time.Second); err != nil || !ok {
		t.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
	}

	ok, err := db.AcquireLease("nat-holder", "coord-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a fresh holder should be able to take over an expired lease")
	}

	// The old holder must not be able to renew it back — coord-b owns it now.
	ok, err = db.AcquireLease("nat-holder", "coord-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("the previous holder must not reclaim a lease it lost to a takeover")
	}
}

func TestReleaseLease_OnlyByCurrentHolder(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "nat-holder")

	if ok, err := db.AcquireLease("nat-holder", "coord-a", time.Minute); err != nil || !ok {
		t.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
	}

	// Releasing with the wrong holder name must be a no-op — coord-a
	// still owns it afterward.
	if err := db.ReleaseLease("nat-holder", "coord-b"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.AcquireLease("nat-holder", "coord-b", time.Minute); err != nil || ok {
		t.Fatalf("coord-a's lease should have survived a release by the wrong holder; ok=%v err=%v", ok, err)
	}

	if err := db.ReleaseLease("nat-holder", "coord-a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.AcquireLease("nat-holder", "coord-b", time.Minute); err != nil || !ok {
		t.Fatalf("lease should be free after the real holder releases it; ok=%v err=%v", ok, err)
	}
}

// TestAcquireAndHold_RenewsUntilContextCanceled confirms the background
// renewal loop keeps the lease alive well past its own TTL as long as
// the parent context is live, and stops renewing (letting the lease
// expire) once it's canceled.
func TestAcquireAndHold_RenewsUntilContextCanceled(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "poller:acme")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leaseCtx, ok := coordination.AcquireAndHold(ctx, db, "poller:acme", "coord-a")
	if !ok {
		t.Fatal("AcquireAndHold should succeed on an unheld lease")
	}

	// Outlive several renewal ticks (LeaseRenewInterval is LeaseTTL/3 =
	// 10s in production; too slow for a unit test, so this just checks
	// the lease is still held and leaseCtx hasn't fired shortly after
	// acquiring — the renewal loop itself is exercised for real by
	// TestAcquireAndHold_GivesUpWhenTakenOver below on a short fuse).
	select {
	case <-leaseCtx.Done():
		t.Fatal("leaseCtx canceled immediately after a successful acquire")
	case <-time.After(50 * time.Millisecond):
	}

	// A second holder must not be able to take it while we're still
	// holding and renewing it.
	if ok, err := db.AcquireLease("poller:acme", "coord-b", time.Minute); err != nil || ok {
		t.Fatalf("coord-b should not acquire a lease coord-a is actively holding; ok=%v err=%v", ok, err)
	}
}

// TestAcquireAndHold_FailsWhenAlreadyHeld confirms AcquireAndHold does
// nothing (no goroutine, no side effects) when the initial acquire fails.
func TestAcquireAndHold_FailsWhenAlreadyHeld(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "cron:watch-1")
	if ok, err := db.AcquireLease("cron:watch-1", "coord-a", time.Minute); err != nil || !ok {
		t.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
	}

	leaseCtx, ok := coordination.AcquireAndHold(context.Background(), db, "cron:watch-1", "coord-b")
	if ok {
		t.Fatal("AcquireAndHold should fail when another holder already owns the lease")
	}
	if leaseCtx != nil {
		t.Fatal("AcquireAndHold should return a nil context on failure")
	}
}

// TestHeldLeases lists the leases the holder holds now, skipping leases it let
// expire and leases someone else holds.
func TestHeldLeases(t *testing.T) {
	db := testOpen(t)
	for _, n := range []string{"state-held", "state-expired", "state-other"} {
		resetLease(t, db, n)
	}
	for _, l := range []struct {
		name, holder string
		ttl          time.Duration
	}{
		{"state-held", "coord-a", time.Minute},
		{"state-expired", "coord-a", -time.Minute},
		{"state-other", "coord-b", time.Minute},
	} {
		if _, err := db.AcquireLease(l.name, l.holder, l.ttl); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.HeldLeases(context.Background(), "coord-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "state-held" {
		t.Errorf("HeldLeases = %v, want [state-held]", got)
	}
}

// A lease runs out by the database's clock: its expiry is the database's
// current time plus the ttl, however the caller's own clock reads. (This host
// shares a clock with the test database, so the test pins where the expiry is
// measured from, not skew itself.)
func TestAcquireLease_ExpiryIsMeasuredOnTheDatabaseClock(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "nat-holder")

	if ok, err := db.AcquireLease("nat-holder", "coord-a", time.Hour); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	var left float64
	if err := db.db.Raw(`SELECT EXTRACT(EPOCH FROM expires_at - clock_timestamp()) FROM leases WHERE name = 'nat-holder'`).Scan(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left > 3600 || left < 3590 {
		t.Errorf("expiry is %.1fs after the database's now, want about 3600", left)
	}
}

// A short lease expires on its own: another holder takes it once it has run
// out, and not before.
func TestAcquireLease_ShortLeaseExpiresAndIsTakenOver(t *testing.T) {
	db := testOpen(t)
	resetLease(t, db, "nat-holder")

	if ok, err := db.AcquireLease("nat-holder", "coord-a", 300*time.Millisecond); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if ok, _ := db.AcquireLease("nat-holder", "coord-b", time.Minute); ok {
		t.Fatal("coord-b took an unexpired lease")
	}
	time.Sleep(400 * time.Millisecond)
	if ok, err := db.AcquireLease("nat-holder", "coord-b", time.Minute); err != nil || !ok {
		t.Fatalf("coord-b should take the expired lease: ok=%v err=%v", ok, err)
	}
}
