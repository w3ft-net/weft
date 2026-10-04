package coordinator

import (
	"context"
	"log"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// shadowKeyPrefix keeps shadow claims out of the real claim keyspace: a
// shadow row can never make a real claim lose.
const shadowKeyPrefix = "shadow:"

// runShadow is the Phase 0 dry run of claim mode (docs/plans/
// cron-claims-rollout.md). While CRON_SHADOW_CLAIMS is on, each tick it
// computes the latest due slot of every lease-mode cron and tries to claim
// "shadow:<key>", logging the outcome. Nothing executes, and lease mode
// keeps running every cron, so the log shows what claim mode would have
// decided: exactly one winner per slot, cadence, lag after FireAt, and
// write rate.
//
// It must never affect lease-mode runs, so it is its own goroutine over a
// snapshot of the watch list, a panic in a tick is recovered, and a failed
// claim write is only logged.
func (ce *CronExecutor) runShadow(ctx context.Context) {
	last := map[uint]int64{}
	ticker := time.NewTicker(cronClaimPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ce.ShadowClaims == nil || !ce.ShadowClaims() {
				// Forget progress while off, so turning it back on starts
				// at the then-current slot instead of resuming mid-gap.
				clear(last)
				continue
			}
			ce.mu.Lock()
			watches := ce.shadowWatches
			ce.mu.Unlock()
			ce.shadowTickSafe(last, watches, time.Now())
		}
	}
}

func (ce *CronExecutor) shadowTickSafe(last map[uint]int64, watches []store.CronWatch, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("cron_claim shadow panic recovered: %v", r)
		}
	}()
	ce.shadowTick(last, watches, now)
}

// shadowTick makes one pass at now. last holds, per watch, the newest slot
// already attempted; a slot whose claim write errored is not recorded, so it
// is retried on the next tick like in claim mode.
func (ce *CronExecutor) shadowTick(last map[uint]int64, watches []store.CronWatch, now time.Time) {
	for _, w := range watches {
		interval := parseSchedule(w.Schedule)
		slot, ok := LatestDueSlot(w.ID, interval, cronSettle, now)
		if !ok {
			continue
		}
		if prev, seen := last[w.ID]; seen && slot.Index <= prev {
			continue
		}
		// Mirror what lease mode would refuse to run.
		if !ce.intervalAllowed(w, interval) {
			last[w.ID] = slot.Index
			continue
		}
		key := shadowKeyPrefix + slot.Key()
		won, err := ce.claimer().ClaimCronSlot(key, ce.Holder)
		lag := now.Sub(slot.FireAt(cronSettle)).Milliseconds()
		if err != nil {
			log.Printf("cron_claim shadow result=error key=%s lag_ms=%d err=%v", key, lag, err)
			continue
		}
		result := "lost"
		if won {
			result = "won"
		}
		log.Printf("cron_claim shadow result=%s key=%s lag_ms=%d", result, key, lag)
		last[w.ID] = slot.Index
	}
}
