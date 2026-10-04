package coordinator

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
)

// CronExecutor runs scheduled cron watches in the background.
// It follows the same goroutine-per-task pattern as the poller.
type CronExecutor struct {
	Store    *store.Store
	Coord    *Coordinator
	Notifier *Notifier
	// Holder identifies this coordinator process for the per-watch
	// lease that guards execution once the coordinator ASG scales past
	// 1 instance — see docs/plans/coordinator-horizontal-scaling.md.
	Holder string
	// Execute fires one run of a watch over tr. Nil means executeWatch;
	// tests substitute it so scheduling can be exercised without a
	// Coordinator.
	Execute func(w store.CronWatch, tr TimeRange)
	// Claims is the slot-claim backend for claim-mode crons. Nil means
	// Store; tests substitute it for fault injection.
	Claims slotClaimer
	// ShadowClaims reports whether the shadow claimer (cron_shadow.go) is
	// enabled. Nil means off. Read every tick, so it doubles as a kill
	// switch.
	ShadowClaims func() bool

	mu      sync.Mutex
	cancels map[uint]*cronRun // watchID → running goroutine
	// shadowWatches is the lease-mode (not tombstoned) enabled crons as of
	// the last reconcile, for the shadow claimer to read.
	shadowWatches []store.CronWatch
}

// cronRun is one running per-watch goroutine. Identity (the pointer) is what
// forgetWatch compares, so a goroutine that outlives its replacement can
// never delete the replacement's entry.
type cronRun struct {
	cancel context.CancelFunc
	claims bool // claim mode (tombstoned) rather than lease mode
}

// Start begins the cron executor. It loads watches from the database,
// spawns a goroutine for each enabled watch, and reconciles every 30 seconds.
func (ce *CronExecutor) Start(ctx context.Context) {
	ce.cancels = make(map[uint]*cronRun)

	// Initial load.
	ce.reconcile(ctx)
	go ce.runShadow(ctx)
	log.Printf("cron: started (%d watches)", len(ce.cancels))

	// Reconcile loop: pick up new/changed/deleted watches.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		prune := time.NewTicker(time.Hour)
		defer prune.Stop()
		for {
			select {
			case <-ctx.Done():
				ce.stopAll()
				return
			case <-ticker.C:
				ce.reconcile(ctx)
			case <-prune.C:
				ce.pruneClaims()
			}
		}
	}()
}

// reconcile syncs running goroutines with the database state.
func (ce *CronExecutor) reconcile(ctx context.Context) {
	var watches []store.CronWatch
	err := ce.Store.DB().WithContext(store.WithoutTenantScope(context.Background())).Where("enabled = ?", true).Find(&watches).Error
	if err != nil {
		log.Printf("cron: failed to list watches: %v", err)
		return
	}

	ce.mu.Lock()
	defer ce.mu.Unlock()

	tombstones, err := ce.Store.CronTombstones()
	if err != nil {
		// Without the tombstone set a claim-mode cron could be started
		// under a lease. Leave everything as it is and retry next tick.
		log.Printf("cron: failed to list tombstones: %v", err)
		return
	}

	// Build set of active watch IDs.
	active := make(map[uint]bool)
	shadow := make([]store.CronWatch, 0, len(watches))
	defer func() { ce.shadowWatches = shadow }()
	for _, w := range watches {
		active[w.ID] = true
		tombstonedAt, claims := tombstones[w.ID]
		if !claims {
			shadow = append(shadow, w)
		}
		run, running := ce.cancels[w.ID]
		if running && run.claims == claims {
			continue
		}
		if running {
			// Mode changed (tombstoned, or rolled back): stop the old
			// goroutine and start one in the new mode.
			run.cancel()
			log.Printf("cron[%s]: mode change, claims=%v", w.Name, claims)
		}
		watchCtx, cancel := context.WithCancel(ctx)
		run = &cronRun{cancel: cancel, claims: claims}
		ce.cancels[w.ID] = run
		if claims {
			go ce.runClaims(watchCtx, w, tombstonedAt, run)
		} else {
			go ce.runWatch(watchCtx, w, run)
		}
	}

	// Stop watches that are no longer enabled.
	for id, run := range ce.cancels {
		if !active[id] {
			run.cancel()
			delete(ce.cancels, id)
		}
	}
}

// stopAll cancels all running watches.
func (ce *CronExecutor) stopAll() {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	for id, run := range ce.cancels {
		run.cancel()
		delete(ce.cancels, id)
	}
}

// forgetWatch removes a watch's tracking entry so the next reconcile tick
// retries it. Needed because reconcile stores the entry and spawns the
// goroutine before knowing whether it'll actually run — if runWatch bails
// (lease held elsewhere) or loses its lease later, reconcile would otherwise
// believe the watch is running forever and never retry, even after the
// other holder dies and the lease frees up. Only removes run's own entry:
// reconcile may already have replaced it (mode change) and that
// replacement must survive.
func (ce *CronExecutor) forgetWatch(id uint, run *cronRun) {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	if ce.cancels[id] == run {
		delete(ce.cancels, id)
	}
}

// runWatch executes a single cron watch on its schedule.
func (ce *CronExecutor) runWatch(ctx context.Context, watch store.CronWatch, run *cronRun) {
	interval := parseSchedule(watch.Schedule)

	// Defensive: skip watches that violate the minimum interval for their tier.
	// This shouldn't happen if saves are validated, but belt-and-suspenders.
	// Checked before acquiring the lease below — no point squatting on it
	// for a watch that's never going to run.
	if !ce.intervalAllowed(watch, interval) {
		return
	}

	// Only one coordinator instance may run a given watch at a time —
	// two instances both firing it on schedule would double-send every
	// alert/notification and double-apply every side effect.
	leaseName := fmt.Sprintf("cron:%d", watch.ID)
	leaseCtx, ok := store.AcquireAndHold(ctx, ce.Store, leaseName, ce.Holder)
	if !ok {
		log.Printf("cron[%s]: lease held by another coordinator instance, not running", watch.Name)
		ce.forgetWatch(watch.ID, run)
		return
	}
	ctx = leaseCtx

	log.Printf("cron[%s]: started (every %s)", watch.Name, interval)

	// Run immediately on first tick.
	ce.fire(watch, watchQueryRange(watch.Schedule, time.Now()))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("cron[%s]: stopped", watch.Name)
			// Reached on lease loss as well as on reconcile/stopAll
			// cancellation; forgetWatch is a no-op unless this goroutine's
			// entry is still the current one.
			ce.forgetWatch(watch.ID, run)
			return
		case <-ticker.C:
			// Reload watch from DB in case pipeline/schedule changed.
			updated, err := ce.Store.GetCronWatch(watch.OrgID, watch.Name)
			if err != nil {
				log.Printf("cron[%s]: failed to reload: %v", watch.Name, err)
				continue
			}
			ce.fire(*updated, watchQueryRange(updated.Schedule, time.Now()))
		}
	}
}

// intervalAllowed reports whether interval meets the minimum for the watch's
// tier. Saves are validated, so this is belt-and-suspenders; it is checked
// before any lease or claim is taken, since there is no point squatting on
// a watch that will never run.
func (ce *CronExecutor) intervalAllowed(watch store.CronWatch, interval time.Duration) bool {
	if tier := ce.tierForWatch(watch); tier != "" {
		minInterval := store.MinWatchInterval(tier)
		if minInterval > 0 && interval < minInterval {
			log.Printf("cron[%s]: skipping — interval %s is below minimum %s for tier %s",
				watch.Name, interval, minInterval, tier)
			return false
		}
	}
	return true
}

// resolvePipeline trims the stored pipeline for execution. /bin/ script
// references no longer need expansion here — Coordinator.query pre-expands
// bash invocations (and binds positionals) before calling Plan, and warp's
// PATH resolution handles direct script invocations at execution time.
// That means `check_errors` (bare name), `check_errors disk.log` (with
// args), and arbitrary compositions (`check_errors | wc -l`) all flow
// through the same server-side machinery.
func (ce *CronExecutor) resolvePipeline(watch store.CronWatch) string {
	return strings.TrimSpace(watch.Pipeline)
}

func (ce *CronExecutor) fire(watch store.CronWatch, tr TimeRange) {
	if ce.Execute != nil {
		ce.Execute(watch, tr)
		return
	}
	ce.executeWatch(watch, tr)
}

// executeWatch runs the watch's pipeline and records the result.
func (ce *CronExecutor) executeWatch(watch store.CronWatch, tr TimeRange) {
	start := time.Now()

	pipeline := ce.resolvePipeline(watch)

	// ExecuteForWatch runs the query and (if the pipeline ends with `| alert`)
	// evaluates the alert in the watch's AlertContext — incident dedup and
	// notifier dispatch both key off the watch identity instead of an
	// interactive-mode synthetic one.
	// No inbound request to propagate a trace ID from — cron/alert
	// firings aren't traced in this pass (see docs/plans/
	// request-tracing.md's "Deferred, not precluded").
	result, err := ce.Coord.ExecuteForWatch(context.Background(), pipeline, tr, &watch)

	duration := time.Since(start)
	status := "ok"
	var stdout, stderr string
	exitCode := 0

	if err != nil {
		status = "error"
		stderr = err.Error()
		exitCode = 1
	} else {
		// Streaming-eligible pipelines return ExecuteResult with only
		// Stream populated; drain to materialize Text/Rows/Stderr for
		// the persistence path below. Buffered paths (alert, redirect,
		// staged compute) short-circuit inside Drain when those fields
		// are already set, so this is cheap in either case.
		drained, drainErr := result.Drain(context.Background())
		if drainErr != nil {
			status = "error"
			stderr = drainErr.Error()
			exitCode = 1
		} else {
			if drained.Text != "" {
				stdout = drained.Text
			}
			if drained.Stderr != "" {
				stderr = drained.Stderr
				status = "alert"
			}
		}
	}

	// Persist result(s). Single-row mode: one cron_results row with
	// tag='' and value auto-parsed from stdout. Multi-row mode (watch has
	// alert_key set): split stdout into lines, extract value from field
	// alert_key and tag from alert_tag, one row per line. Lines whose -k
	// field isn't numeric are persisted with Value=NULL so the debugging
	// trail stays intact; aggregates ignore NULLs.
	if saveErr := ce.persistResults(watch, start, stdout, stderr, exitCode, duration); saveErr != nil {
		log.Printf("cron[%s]: failed to save result: %v", watch.Name, saveErr)
	}

	// Update watch status.
	if updateErr := func(id uint, status string) error {
		now := time.Now()
		return ce.Store.DB().WithContext(store.WithoutTenantScope(context.Background())).Model(&store.CronWatch{}).Where("id = ?", id).Updates(map[string]any{
			"last_run_at": &now,
			"last_status": status,
		}).Error
	}(watch.ID, status); updateErr != nil {
		log.Printf("cron[%s]: failed to update run status: %v", watch.Name, updateErr)
	}

	log.Printf("cron[%s]: %s (%dms, stdout=%d bytes, stderr=%d bytes)",
		watch.Name, status, duration.Milliseconds(), len(stdout), len(stderr))
}

// persistResults writes one or more cron_results rows for this tick. In
// single-row mode (watch.AlertKey nil) we write a single row with tag=”
// and let SaveCronResult auto-parse Stdout into Value. In multi-row mode
// we split stdout into lines, resolve Value from the watch's -k spec
// (positional or JSON path) and Tag from the -t spec, and write one row
// per non-blank line. Lines whose value field isn't numeric are still
// persisted — with Value=NULL — so the history stays a faithful record;
// aggregates ignore NULLs. Stderr and exit_code are only attached to the
// first row in multi-row mode (they describe the whole tick, not a row).
func (ce *CronExecutor) persistResults(watch store.CronWatch, ranAt time.Time, stdout, stderr string, exitCode int, duration time.Duration) error {
	if watch.AlertKey == nil {
		result := &store.CronResult{
			CronWatchID: watch.ID,
			RanAt:       ranAt,
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    exitCode,
			DurationMs:  duration.Milliseconds(),
		}
		if result.Value == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(result.Stdout), 64); err == nil {
				result.Value = &v
			}
		}
		return ce.Store.DB().Create(result).Error
	}

	keySpec, err := shellcore.ParseFieldSpec(*watch.AlertKey)
	if err != nil {
		return fmt.Errorf("alert_key %q: %w", *watch.AlertKey, err)
	}
	var tagSpec shellcore.FieldSpec
	if watch.AlertTag != nil {
		tagSpec, err = shellcore.ParseFieldSpec(*watch.AlertTag)
		if err != nil {
			return fmt.Errorf("alert_tag %q: %w", *watch.AlertTag, err)
		}
	}

	var rows []store.CronResult
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		r := store.CronResult{
			CronWatchID: watch.ID,
			RanAt:       ranAt,
			Stdout:      line,
			ExitCode:    exitCode,
			DurationMs:  duration.Milliseconds(),
		}
		if first {
			r.Stderr = stderr
			first = false
		}
		if val, err := shellcore.ResolveField(keySpec, line); err == nil && val != "" {
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				r.Value = &v
			}
		}
		if !tagSpec.IsZero() {
			if t, err := shellcore.ResolveField(tagSpec, line); err == nil {
				r.Tag = t
			}
		}
		rows = append(rows, r)
	}

	// Empty multi-row output = no rows written. That's fine; the watch's
	// "last run" timestamp still updates via UpdateCronWatchRun. If we want
	// an audit trail of empty ticks in multi-row mode we can add a sentinel
	// row later.
	return ce.Store.SaveCronResults(rows)
}

// tierForWatch looks up the subscription tier for the customer that owns the watch.
func (ce *CronExecutor) tierForWatch(watch store.CronWatch) store.Tier {
	cust, err := ce.Store.GetOrgByID(watch.OrgID)
	if err != nil {
		return ""
	}
	sub, err := ce.Store.ActiveSubscription(cust.ID)
	if err != nil || sub == nil {
		return store.TierTrial
	}
	return store.Tier(sub.Tier)
}

// parseSchedule parses a schedule string into a duration.
// Delegates to store.ParseScheduleInterval.
func parseSchedule(schedule string) time.Duration {
	return store.ParseScheduleInterval(schedule)
}

// watchQueryRange is the time window a cron run queries: exactly the gap
// since the previous fire, [now-interval, now]. The interval is the same
// value runWatch tickers on (parseSchedule), so this tiles fire-to-fire
// with no gap and never exceeds the interval — which keeps a cron from
// reaching shards older than its own cadence (notably the archived
// cold-Lambda shards). Previously this was hardcoded to an all-time
// [2000..2100] window, which fanned every tick onto every in-retention
// shard. See cron_window_test.go.
func watchQueryRange(schedule string, now time.Time) TimeRange {
	return TimeRange{Start: now.Add(-parseSchedule(schedule)), End: now}
}
