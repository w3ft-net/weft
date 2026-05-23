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

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/store"
)

// CronExecutor runs scheduled cron watches in the background.
// It follows the same goroutine-per-task pattern as the poller.
type CronExecutor struct {
	Store    *store.Store
	Coord    *Coordinator
	Notifier *Notifier

	mu      sync.Mutex
	cancels map[uint]context.CancelFunc // watchID → cancel
}

// Start begins the cron executor. It loads watches from the database,
// spawns a goroutine for each enabled watch, and reconciles every 30 seconds.
func (ce *CronExecutor) Start(ctx context.Context) {
	ce.cancels = make(map[uint]context.CancelFunc)

	// Initial load.
	ce.reconcile(ctx)
	log.Printf("cron: started (%d watches)", len(ce.cancels))

	// Reconcile loop: pick up new/changed/deleted watches.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				ce.stopAll()
				return
			case <-ticker.C:
				ce.reconcile(ctx)
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

	// Build set of active watch IDs.
	active := make(map[uint]bool)
	for _, w := range watches {
		active[w.ID] = true
		if _, running := ce.cancels[w.ID]; !running {
			// Start new watch.
			watchCtx, cancel := context.WithCancel(ctx)
			ce.cancels[w.ID] = cancel
			go ce.runWatch(watchCtx, w)
		}
	}

	// Stop watches that are no longer enabled.
	for id, cancel := range ce.cancels {
		if !active[id] {
			cancel()
			delete(ce.cancels, id)
		}
	}
}

// stopAll cancels all running watches.
func (ce *CronExecutor) stopAll() {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	for id, cancel := range ce.cancels {
		cancel()
		delete(ce.cancels, id)
	}
}

// runWatch executes a single cron watch on its schedule.
func (ce *CronExecutor) runWatch(ctx context.Context, watch store.CronWatch) {
	interval := parseSchedule(watch.Schedule)

	// Defensive: skip watches that violate the minimum interval for their tier.
	// This shouldn't happen if saves are validated, but belt-and-suspenders.
	if tier := ce.tierForWatch(watch); tier != "" {
		minInterval := store.MinWatchInterval(tier)
		if minInterval > 0 && interval < minInterval {
			log.Printf("cron[%s]: skipping — interval %s is below minimum %s for tier %s",
				watch.Name, interval, minInterval, tier)
			return
		}
	}

	log.Printf("cron[%s]: started (every %s)", watch.Name, interval)

	// Run immediately on first tick.
	ce.executeWatch(watch)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("cron[%s]: stopped", watch.Name)
			return
		case <-ticker.C:
			// Reload watch from DB in case pipeline/schedule changed.
			updated, err := ce.Store.GetCronWatch(watch.CustomerID, watch.Name)
			if err != nil {
				log.Printf("cron[%s]: failed to reload: %v", watch.Name, err)
				continue
			}
			ce.executeWatch(*updated)
		}
	}
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

// executeWatch runs the watch's pipeline and records the result.
func (ce *CronExecutor) executeWatch(watch store.CronWatch) {
	start := time.Now()

	pipeline := ce.resolvePipeline(watch)

	tr := watchQueryRange(watch.Schedule, start)
	// ExecuteForWatch runs the query and (if the pipeline ends with `| alert`)
	// evaluates the alert in the watch's AlertContext — incident dedup and
	// notifier dispatch both key off the watch identity instead of an
	// interactive-mode synthetic one.
	result, err := ce.Coord.ExecuteForWatch(pipeline, tr, &watch)

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
	cust, err := ce.Store.GetCustomerByID(watch.CustomerID)
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
