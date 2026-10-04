package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

func shadowOwners(f *fakeClaimer) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.owners {
		out[k] = v
	}
	return out
}

// Coordinators shadow-claiming the same slots: exactly one winner per slot,
// nothing executes, and shadow rows never block a real claim.
func TestShadowTickOneWinnerPerSlotAndNeverExecutes(t *testing.T) {
	env := newClaimEnv(t, "shadow-winners")
	watches := []store.CronWatch{env.watch}
	a, b := env.executor("a"), env.executor("b")
	lastA, lastB := map[uint]int64{}, map[uint]int64{}

	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for at := time.Duration(0); at < time.Hour; at += time.Second {
		a.shadowTick(lastA, watches, t0.Add(at))
		b.shadowTick(lastB, watches, t0.Add(at))
	}

	owners := shadowOwners(env.claims)
	if len(owners) < 11 { // a 5m cron: ~12 slots/hour
		t.Fatalf("only %d shadow slots in an hour", len(owners))
	}
	for k := range owners {
		if !strings.HasPrefix(k, shadowKeyPrefix) {
			t.Fatalf("shadow claim used a non-shadow key %q", k)
		}
	}
	if n := len(env.runs()); n != 0 {
		t.Fatalf("shadow mode executed %d runs", n)
	}

	// A real claim for the same slot is unaffected by the shadow row.
	slot, _ := LatestDueSlot(env.watch.ID, 5*time.Minute, cronSettle, t0.Add(time.Hour))
	if won, err := env.claims.ClaimCronSlot(slot.Key(), "a"); err != nil || !won {
		t.Fatalf("real claim blocked by shadow row: won=%v err=%v", won, err)
	}
}

// A failed claim write is logged, not recorded, and retried next tick.
func TestShadowTickRetriesAfterClaimError(t *testing.T) {
	env := newClaimEnv(t, "shadow-retry")
	ce := env.executor("a")
	last := map[uint]int64{}
	now := time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC)

	env.claims.err = errors.New("db down")
	ce.shadowTick(last, []store.CronWatch{env.watch}, now)
	if len(shadowOwners(env.claims)) != 0 {
		t.Fatal("claim recorded despite error")
	}
	env.claims.err = nil
	ce.shadowTick(last, []store.CronWatch{env.watch}, now.Add(time.Second))
	if len(shadowOwners(env.claims)) != 1 {
		t.Fatal("slot was not retried after the claim store recovered")
	}
}

type panicClaimer struct{}

func (panicClaimer) ClaimCronSlot(string, string) (bool, error) { panic("boom") }

// A panic in the shadow path must not escape (it shares a process with
// lease-mode crons).
func TestShadowTickSafeRecoversPanic(t *testing.T) {
	env := newClaimEnv(t, "shadow-panic")
	ce := env.executor("a")
	ce.Claims = panicClaimer{}
	ce.shadowTickSafe(map[uint]int64{}, []store.CronWatch{env.watch}, time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC))
}

// Flag off (the default): the loop writes nothing. Flag on: it does.
func TestRunShadowHonorsFlag(t *testing.T) {
	env := newClaimEnv(t, "shadow-flag")
	ce := env.executor("a")
	var on atomic.Bool
	ce.ShadowClaims = on.Load
	ce.shadowWatches = []store.CronWatch{env.watch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ce.runShadow(ctx)

	time.Sleep(2500 * time.Millisecond)
	if n := len(shadowOwners(env.claims)); n != 0 {
		t.Fatalf("shadow wrote %d claims with the flag off", n)
	}
	on.Store(true)
	if err := waitFor(3*time.Second, func() bool { return len(shadowOwners(env.claims)) == 1 }); err != nil {
		t.Fatal("shadow did not start claiming after the flag was turned on")
	}
}
