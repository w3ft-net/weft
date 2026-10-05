package coordinator

import (
	"context"
	"log"
	"time"

	"github.com/graybeardcorp/weft/store"
)

const (
	// cronSettle is how long after a window closes a cron waits before
	// querying it, so rows still in flight from ingest can land. weftd and
	// the coordinator's 2s shard batching need only a few seconds;
	// CloudWatch-sourced rows can lag 5-60s (docs/ingestion-weftd.md), and
	// a window queried early is undercounted for good. 30s covers most of
	// that; the 60s tail needs a per-source settle (see
	// docs/plans/cron-claims-rollout.md).
	cronSettle = 30 * time.Second
	// cronClaimPoll is how often a claim-mode cron checks whether a new
	// slot is due. Checking is a clock comparison; the database is only
	// touched when a new slot comes due.
	cronClaimPoll = time.Second
	// cronClaimRetention is how long claim rows are kept for forensics.
	cronClaimRetention = 24 * time.Hour
)

// slotClaimer is the exactly-once primitive claim mode runs on.
// *store.Store implements it.
type slotClaimer interface {
	ClaimCronSlot(key, holder string) (won bool, err error)
}

func (ce *CronExecutor) claimer() slotClaimer {
	if ce.Claims != nil {
		return ce.Claims
	}
	return ce.Store
}

// cronClaimState is what a claim-mode loop remembers between ticks.
type cronClaimState struct {
	watch store.CronWatch
	// lastSlot is the newest slot already decided (won, lost, or skipped
	// in the quiet period). A slot that errored is not recorded, so the
	// next tick retries it.
	lastSlot int64
	seen     bool
}

// runClaims runs one cron: every cronClaimPoll it asks whether a new slot is
// due, and runs it if this coordinator wins the slot's claim. Runs until ctx
// is cancelled (reconcile, when the cron is disabled or deleted).
func (ce *CronExecutor) runClaims(ctx context.Context, watch store.CronWatch) {
	// Defensive: skip watches that violate the minimum interval for their
	// tier. Saves are validated, so this is belt-and-suspenders.
	if !ce.intervalAllowed(watch, parseSchedule(watch.Schedule)) {
		return
	}
	log.Printf("cron[%s]: started (every %s)", watch.Name, parseSchedule(watch.Schedule))

	st := &cronClaimState{watch: watch}
	ticker := time.NewTicker(cronClaimPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("cron[%s]: stopped", watch.Name)
			return
		case <-ticker.C:
			ce.claimTick(st, time.Now())
		}
	}
}

// claimTick is one scheduling decision at time now. It never runs a slot
// without holding its claim: a failed claim write means no run, and the
// slot is retried on the next tick (fail closed — the worst case is a
// skipped slot, never a duplicate).
func (ce *CronExecutor) claimTick(st *cronClaimState, now time.Time) {
	w := st.watch
	interval := parseSchedule(w.Schedule)
	slot, ok := LatestDueSlot(w.ID, interval, cronSettle, now)
	if !ok || (st.seen && slot.Index <= st.lastSlot) {
		return
	}
	decided := func() { st.lastSlot, st.seen = slot.Index, true }

	// A cron created after this slot's window closed has nothing to report
	// for it, and running it would evaluate (and could alert on) data from
	// before the cron existed. Its first run is the next slot.
	if slot.Boundary().Before(w.CreatedAt) {
		decided()
		return
	}

	// Pick up pipeline/schedule edits before claiming. A schedule change
	// changes the slot grid, so re-evaluate on the next tick rather than
	// claim a slot from the old one.
	updated, err := ce.Store.GetCronWatch(w.OrgID, w.Name)
	if err != nil {
		log.Printf("cron[%s]: failed to reload, skipping tick: %v", w.Name, err)
		return
	}
	if parseSchedule(updated.Schedule) != interval {
		log.Printf("cron[%s]: schedule changed to %q, re-slotting", w.Name, updated.Schedule)
		st.watch, st.seen = *updated, false
		return
	}
	st.watch = *updated

	won, err := ce.claimer().ClaimCronSlot(slot.Key(), ce.Holder)
	lag := now.Sub(slot.FireAt(cronSettle)).Milliseconds()
	switch {
	case err != nil:
		log.Printf("cron_claim result=error key=%s lag_ms=%d err=%v", slot.Key(), lag, err)
		return
	case !won:
		log.Printf("cron_claim result=lost key=%s lag_ms=%d", slot.Key(), lag)
		decided()
		return
	}
	log.Printf("cron_claim result=won key=%s lag_ms=%d", slot.Key(), lag)
	decided()
	ce.fire(st.watch, slot.Window())
}

// pruneClaims drops claim rows past cronClaimRetention.
func (ce *CronExecutor) pruneClaims() {
	n, err := ce.Store.DeleteCronClaimsBefore(time.Now().Add(-cronClaimRetention))
	if err != nil {
		log.Printf("cron: claim cleanup failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("cron: pruned %d old claims", n)
	}
}
