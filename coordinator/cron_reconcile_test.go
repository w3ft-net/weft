package coordinator

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// Every enabled cron runs under claims by default: reconcile starts a claim
// loop for it without any tombstone, and never takes a per-cron lease. A
// brand-new cron has no tombstone, so this is what makes new crons safe.
func TestReconcileRunsEveryEnabledCronUnderClaims(t *testing.T) {
	env := newClaimEnv(t, "reconcile-default")
	ce := env.executor("a")
	ce.cancels = map[uint]context.CancelFunc{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ce.reconcile(ctx)

	ce.mu.Lock()
	_, running := ce.cancels[env.watch.ID]
	ce.mu.Unlock()
	if !running {
		t.Fatal("an enabled cron with no tombstone was not started")
	}

	// Give a lease-mode goroutine time to take its lease (it does so
	// immediately); under claims it never does.
	time.Sleep(1500 * time.Millisecond)
	var leases int64
	env.db.DB().Raw(`SELECT count(*) FROM leases WHERE name = ?`, "cron:"+strconv.FormatUint(uint64(env.watch.ID), 10)).Scan(&leases)
	if leases != 0 {
		t.Fatalf("cron took a per-cron lease (%d row): the lease path is still in use", leases)
	}
}

// Disabling a cron stops its claim loop; enabling it again restarts it.
func TestReconcileStopsDisabledCronsAndRestartsEnabledOnes(t *testing.T) {
	env := newClaimEnv(t, "reconcile-toggle")
	ce := env.executor("a")
	ce.cancels = map[uint]context.CancelFunc{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adb := env.db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	tracked := func() bool {
		ce.mu.Lock()
		defer ce.mu.Unlock()
		_, ok := ce.cancels[env.watch.ID]
		return ok
	}

	ce.reconcile(ctx)
	if !tracked() {
		t.Fatal("enabled cron not started")
	}
	adb.Model(&store.CronWatch{}).Where("id = ?", env.watch.ID).Update("enabled", false)
	ce.reconcile(ctx)
	if tracked() {
		t.Fatal("disabled cron still running")
	}
	adb.Model(&store.CronWatch{}).Where("id = ?", env.watch.ID).Update("enabled", true)
	ce.reconcile(ctx)
	if !tracked() {
		t.Fatal("re-enabled cron not restarted")
	}
}
