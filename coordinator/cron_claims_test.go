package coordinator

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// fakeClaimer is an in-memory slotClaimer shared by several simulated
// coordinators. err, when set, fails every claim.
type fakeClaimer struct {
	mu     sync.Mutex
	owners map[string]string
	err    error
}

func (f *fakeClaimer) ClaimCronSlot(key, holder string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if f.owners == nil {
		f.owners = map[string]string{}
	}
	if _, taken := f.owners[key]; taken {
		return false, nil
	}
	f.owners[key] = holder
	return true, nil
}

type firedRun struct {
	holder string
	tr     TimeRange
}

type claimEnv struct {
	db      *store.Store
	watch   store.CronWatch
	claims  *fakeClaimer
	mu      sync.Mutex
	fired   []firedRun
	cleanup func()
}

// newClaimEnv creates a committed cron watch (the claim tick reloads it
// from the database) with a fake claim backend.
func newClaimEnv(t *testing.T, name string) *claimEnv {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	adb := db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	org, err := db.FindOrCreateOrg("cron-claims-test")
	if err != nil {
		t.Fatal(err)
	}
	w := store.CronWatch{OrgID: org.ID, Name: name, Schedule: "every 5m", Pipeline: "true", Enabled: true}
	if err := adb.Create(&w).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adb.Exec(`DELETE FROM leases WHERE name = ?`, store.CronLeaseName(w.ID))
		adb.Unscoped().Delete(&w)
		// Other coordinator tests assume the pooled fixture DB holds a
		// single org, so don't leave ours behind.
		adb.Exec(`DELETE FROM organizations WHERE id = ?`, org.ID)
		db.Close()
	})
	return &claimEnv{db: db, watch: w, claims: &fakeClaimer{}}
}

func (e *claimEnv) executor(holder string) *CronExecutor {
	return &CronExecutor{
		Store: e.db, Holder: holder, Claims: e.claims,
		Execute: func(_ store.CronWatch, tr TimeRange) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.fired = append(e.fired, firedRun{holder, tr})
		},
	}
}

func (e *claimEnv) runs() []firedRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]firedRun(nil), e.fired...)
	sort.Slice(out, func(i, j int) bool { return out[i].tr.Start.Before(out[j].tr.Start) })
	return out
}

// Several coordinators ticking against one claim backend, through a
// coordinator restart: every slot runs exactly once, and consecutive
// windows tile with no gap or overlap.
func TestClaimTicksRunEachSlotOnceAcrossCoordinators(t *testing.T) {
	env := newClaimEnv(t, "claims-sim")
	interval := 5 * time.Minute
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	tombstoned := t0.Add(-time.Hour)

	type coord struct {
		ce *CronExecutor
		st *cronClaimState
	}
	mk := func(h string) *coord {
		return &coord{env.executor(h), &cronClaimState{watch: env.watch}}
	}
	coords := []*coord{mk("a"), mk("b"), mk("c")}

	const sim = 2 * time.Hour
	for at := time.Duration(0); at < sim; at += time.Second {
		now := t0.Add(at)
		// Halfway through, coordinator a is replaced by a fresh process
		// that has no memory of which slots were decided.
		if at == sim/2 {
			coords[0] = mk("a2")
		}
		for _, c := range coords {
			c.ce.claimTick(c.st, tombstoned, now)
		}
	}

	runs := env.runs()
	// Slots whose fire time falls in [t0, t0+2h): boundaries every 5m,
	// shifted by settle+jitter; the first partial slot may or may not
	// have fired before t0, so check the interior by tiling.
	if len(runs) < int(sim/interval)-1 {
		t.Fatalf("only %d runs in %s", len(runs), sim)
	}
	for i := 1; i < len(runs); i++ {
		prev, cur := runs[i-1].tr, runs[i].tr
		if want := prev.End.Add(time.Microsecond); !cur.Start.Equal(want) {
			t.Fatalf("run %d window starts %v, want %v (duplicate or gap after %v)", i, cur.Start, want, prev.End)
		}
	}
}

// A failed claim write must never run the slot, and the slot is retried.
func TestClaimTickFailsClosedAndRetries(t *testing.T) {
	env := newClaimEnv(t, "claims-failclosed")
	ce := env.executor("a")
	st := &cronClaimState{watch: env.watch}
	tombstoned := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	// Past slot 12:05's fire time (boundary + settle + jitter < 60s).
	now := time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC)

	env.claims.err = errors.New("db down")
	for i := 0; i < 3; i++ {
		ce.claimTick(st, tombstoned, now.Add(time.Duration(i)*time.Second))
	}
	if n := len(env.runs()); n != 0 {
		t.Fatalf("ran %d times with the claim store failing", n)
	}

	env.claims.err = nil
	ce.claimTick(st, tombstoned, now.Add(4*time.Second))
	ce.claimTick(st, tombstoned, now.Add(5*time.Second))
	if n := len(env.runs()); n != 1 {
		t.Fatalf("want exactly 1 run after the store recovered, got %d", n)
	}
}

// Slots that fire inside the quiet period after the tombstone are skipped,
// and not claimed.
func TestClaimTickQuietPeriodAfterTombstone(t *testing.T) {
	env := newClaimEnv(t, "claims-quiet")
	ce := env.executor("a")
	st := &cronClaimState{watch: env.watch}

	slot, _ := LatestDueSlot(env.watch.ID, 5*time.Minute, cronSettle, time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC))
	fireAt := slot.FireAt(cronSettle)
	// Tombstoned 1s before the slot fires: inside the quiet period.
	ce.claimTick(st, fireAt.Add(-time.Second), fireAt.Add(time.Second))
	if n := len(env.runs()); n != 0 {
		t.Fatalf("ran %d times inside the quiet period", n)
	}
	if len(env.claims.owners) != 0 {
		t.Fatalf("quiet-period slot was claimed: %v", env.claims.owners)
	}

	// The next slot is past the quiet period and runs.
	ce.claimTick(st, fireAt.Add(-time.Second), fireAt.Add(5*time.Minute+time.Second))
	if n := len(env.runs()); n != 1 {
		t.Fatalf("want 1 run after the quiet period, got %d", n)
	}
}

// Through the real reconcile: a lease-mode cron is switched to claim mode
// by a tombstone without a second run, and rolled back by the rollback
// state; the lease path cannot start until the rollback window passes.
func TestReconcileFollowsTombstoneAndRollback(t *testing.T) {
	env := newClaimEnv(t, "claims-transitions")
	id := env.watch.ID
	ce := env.executor("a")
	ce.cancels = map[uint]*cronRun{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mode := func() (claims, tracked bool) {
		ce.mu.Lock()
		defer ce.mu.Unlock()
		r, ok := ce.cancels[id]
		return ok && r.claims, ok
	}
	runsFor := func() int { return len(env.runs()) }

	ce.reconcile(ctx)
	if err := waitFor(5*time.Second, func() bool { return runsFor() == 1 }); err != nil {
		t.Fatal("lease-mode cron did not run its first tick")
	}
	if c, _ := mode(); c {
		t.Fatal("untombstoned cron is in claim mode")
	}

	if err := env.db.TombstoneCronLease(id); err != nil {
		t.Fatal(err)
	}
	ce.reconcile(ctx)
	if c, ok := mode(); !ok || !c {
		t.Fatalf("after tombstone: claims=%v tracked=%v, want claim mode", c, ok)
	}
	time.Sleep(1500 * time.Millisecond) // claim loop ticks; slot is inside the quiet period
	if n := runsFor(); n != 1 {
		t.Fatalf("switching to claim mode ran the cron again: %d runs", n)
	}

	if ok, err := env.db.RollbackCronTombstone(id); err != nil || !ok {
		t.Fatalf("rollback: ok=%v err=%v", ok, err)
	}
	ce.reconcile(ctx)
	// Back in lease mode, but the lease is denied for the rollback
	// window, so the goroutine bails and is forgotten, retried later.
	if err := waitFor(5*time.Second, func() bool { _, tracked := mode(); return !tracked }); err != nil {
		t.Fatal("rolled-back cron was not released while its lease is denied")
	}
	if n := runsFor(); n != 1 {
		t.Fatalf("lease mode ran during the rollback window: %d runs", n)
	}
}
