package coordinator

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// A coordinator that loses a cron's lease must forget the watch so a later
// reconcile can restart it once the lease is obtainable again. Before the
// fix, runWatch returned on lease loss with ce.cancels[id] still set, so
// reconcile believed the cron was running and never restarted it.
func TestCronLeaseLossThenRestart(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	adb := db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	org, err := db.FindOrCreateOrg("cron-lease-loss-test")
	if err != nil {
		t.Fatal(err)
	}
	w := store.CronWatch{OrgID: org.ID, Name: "lease-loss", Schedule: "every 5m", Pipeline: "true", Enabled: true}
	if err := adb.Create(&w).Error; err != nil {
		t.Fatal(err)
	}
	leaseName := "cron:" + strconv.FormatUint(uint64(w.ID), 10)
	t.Cleanup(func() {
		adb.Exec(`DELETE FROM leases WHERE name = ?`, leaseName)
		adb.Unscoped().Delete(&w)
		// Other coordinator tests assume the pooled fixture DB holds a
		// single org, so don't leave ours behind.
		adb.Exec(`DELETE FROM organizations WHERE id = ?`, org.ID)
	})

	var runs atomic.Int32
	ce := &CronExecutor{
		Store:   db,
		Holder:  "me",
		Execute: func(store.CronWatch, TimeRange) { runs.Add(1) },
		cancels: map[uint]*cronRun{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ce.mu.Lock()
	wctx, wcancel := context.WithCancel(ctx)
	run := &cronRun{cancel: wcancel}
	ce.cancels[w.ID] = run
	ce.mu.Unlock()
	go ce.runWatch(wctx, w, run)
	mustWait(t, 5*time.Second, func() bool { return runs.Load() == 1 }, "first run")

	// Another coordinator takes the lease out from under us; our next
	// renewal (within LeaseRenewInterval) loses it and runWatch exits.
	if err := adb.Exec(`UPDATE leases SET holder = 'other', expires_at = now() + interval '12 seconds' WHERE name = ?`, leaseName).Error; err != nil {
		t.Fatal(err)
	}
	mustWait(t, store.LeaseRenewInterval+5*time.Second, func() bool {
		ce.mu.Lock()
		defer ce.mu.Unlock()
		_, tracked := ce.cancels[w.ID]
		return !tracked
	}, "lost-lease watch forgotten by executor")

	// Once the other holder's lease expires, reconcile restarts the cron.
	time.Sleep(4 * time.Second)
	ce.reconcile(ctx)
	mustWait(t, 5*time.Second, func() bool { return runs.Load() == 2 }, "restart after lease frees")
}

func mustWait(t *testing.T, d time.Duration, ok func() bool, what string) {
	t.Helper()
	if err := waitFor(d, ok); err != nil {
		t.Fatalf("timed out waiting for: %s", what)
	}
}
