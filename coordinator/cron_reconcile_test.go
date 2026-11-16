package coordinator

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/shape"
)

// Every enabled cron runs under claims: reconcile starts a claim loop for it
// without any tombstone. A brand-new cron has none, so this is what makes new
// crons safe.
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
}

// A cron that is moved or renamed keeps its ID but not its address. Its loop
// looks the cron up by address each tick, so reconcile restarts the loop when
// the address changes; left running it reports "record not found" every tick
// and the cron never fires again.
func TestReconcileRestartsALoopWhoseCronMoved(t *testing.T) {
	env := newClaimEnv(t, "reconcile-moved")
	ce := env.executor("a")
	ce.cancels = map[uint]context.CancelFunc{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ce.reconcile(ctx)
	stopped := make(chan struct{})
	ce.mu.Lock()
	ce.cancels[env.watch.ID] = func() { close(stopped) }
	ce.mu.Unlock()

	if err := env.crons.MoveWatch(shape.Added{}, 0, env.watch.Name, 0, env.watch.Name+"-moved"); err != nil {
		t.Fatal(err)
	}
	ce.reconcile(ctx)

	select {
	case <-stopped:
	default:
		t.Fatal("the loop that looks the cron up at its old address was left running")
	}
	ce.mu.Lock()
	_, running := ce.cancels[env.watch.ID]
	ce.mu.Unlock()
	if !running {
		t.Fatal("no loop runs the cron at its new address")
	}
}

// Disabling a cron stops its claim loop; enabling it again restarts it.
func TestReconcileStopsDisabledCronsAndRestartsEnabledOnes(t *testing.T) {
	env := newClaimEnv(t, "reconcile-toggle")
	ce := env.executor("a")
	ce.cancels = map[uint]context.CancelFunc{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracked := func() bool {
		ce.mu.Lock()
		defer ce.mu.Unlock()
		_, ok := ce.cancels[env.watch.ID]
		return ok
	}
	setEnabled := func(on bool) {
		t.Helper()
		spec := env.watch.WatchSpec
		spec.Enabled = on
		if err := env.crons.SaveWatch(shape.Added{}, 0, "", env.watch.Name, spec); err != nil {
			t.Fatal(err)
		}
	}

	ce.reconcile(ctx)
	if !tracked() {
		t.Fatal("enabled cron not started")
	}
	setEnabled(false)
	ce.reconcile(ctx)
	if tracked() {
		t.Fatal("disabled cron still running")
	}
	setEnabled(true)
	ce.reconcile(ctx)
	if !tracked() {
		t.Fatal("re-enabled cron not restarted")
	}
}
