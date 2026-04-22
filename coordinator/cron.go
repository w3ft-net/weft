package coordinator

import (
	"bufio"
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevlarcon/weft/store"
)

// CronExecutor runs scheduled cron watches in the background.
// It follows the same goroutine-per-task pattern as the poller.
type CronExecutor struct {
	Store    *store.Store
	Coord    *Coordinator
	Notifier *Notifier

	mu       sync.Mutex
	cancels  map[uint]context.CancelFunc // watchID → cancel
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
	watches, err := ce.Store.ListEnabledCronWatches()
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

// resolvePipeline expands function references and normalizes /log/ paths
// so the coordinator's planner can parse the pipeline.
func (ce *CronExecutor) resolvePipeline(watch store.CronWatch) string {
	pipeline := strings.TrimSpace(watch.Pipeline)

	// If the pipeline is a bare name (no spaces, no pipes), check if it's a /bin/ function.
	if !strings.Contains(pipeline, " ") && !strings.Contains(pipeline, "|") {
		fn, err := ce.Store.GetShellFunction(watch.CustomerID, pipeline)
		if err == nil {
			pipeline = strings.TrimSpace(fn.Source)
		}
	}

	return pipeline
}

// splitAlertStage splits a pipeline at "| alert". Everything before runs
// as a coordinator query; the alert part runs through a local warp interpreter.
func splitAlertStage(pipeline string) (queryPart, alertPart string) {
	idx := strings.Index(pipeline, "| alert")
	if idx == -1 {
		return pipeline, ""
	}
	return strings.TrimSpace(pipeline[:idx]), strings.TrimSpace(pipeline[idx+2:])
}

// executeWatch runs the watch's pipeline and records the result.
func (ce *CronExecutor) executeWatch(watch store.CronWatch) {
	start := time.Now()

	pipeline := ce.resolvePipeline(watch)
	queryPart, alertPart := splitAlertStage(pipeline)

	tr := TimeRange{
		Start: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	result, err := ce.Coord.Query(queryPart, tr)

	duration := time.Since(start)
	status := "ok"
	var stdout, stderr string
	exitCode := 0

	if err != nil {
		status = "error"
		stderr = err.Error()
		exitCode = 1
	} else if result.Text != "" {
		stdout = result.Text
	} else if len(result.Rows) > 0 {
		var sb strings.Builder
		for _, row := range result.Rows {
			sb.WriteString(row.Raw)
			sb.WriteByte('\n')
		}
		stdout = sb.String()
	}

	// Run alert stage if present. The alert built-in evaluates the condition,
	// updates the incident ledger, and dispatches through the Notifier itself
	// when a --notify alias is set on the command — no more post-hoc
	// stderr→notify magic at this layer.
	if alertPart != "" && exitCode == 0 {
		alertCtx := &AlertContext{
			Store:      ce.Store,
			Watch:      &watch,
			Notifier:   ce.Notifier,
			CustomerID: watch.CustomerID,
		}
		alertArgs := strings.Fields(alertPart)
		stdinReader := strings.NewReader(stdout)
		var alertStdout, alertStderr strings.Builder
		AlertCommand(alertArgs, stdinReader, &alertStdout, &alertStderr, alertCtx)
		if alertStderr.Len() > 0 {
			stderr = alertStderr.String()
			status = "alert"
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
	if updateErr := ce.Store.UpdateCronWatchRun(watch.ID, status); updateErr != nil {
		log.Printf("cron[%s]: failed to update run status: %v", watch.Name, updateErr)
	}

	log.Printf("cron[%s]: %s (%dms, stdout=%d bytes, stderr=%d bytes)",
		watch.Name, status, duration.Milliseconds(), len(stdout), len(stderr))
}

// persistResults writes one or more cron_results rows for this tick. In
// single-row mode (watch.AlertKey nil) we write a single row with tag='' and
// let SaveCronResult auto-parse Stdout into Value. In multi-row mode we
// split stdout into lines, extract value from field AlertKey and tag from
// AlertTag, and write one row per non-blank line. Lines whose value field
// is non-numeric are still persisted — with Value=NULL — so the history
// stays a faithful record; aggregates ignore NULLs. Stderr and exit_code
// are only attached to the first row in multi-row mode (they describe the
// whole tick, not a specific row).
func (ce *CronExecutor) persistResults(watch store.CronWatch, ranAt time.Time, stdout, stderr string, exitCode int, duration time.Duration) error {
	if watch.AlertKey == nil {
		return ce.Store.SaveCronResult(&store.CronResult{
			CronWatchID: watch.ID,
			RanAt:       ranAt,
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    exitCode,
			DurationMs:  duration.Milliseconds(),
		})
	}

	k := *watch.AlertKey
	tagField := 0
	if watch.AlertTag != nil {
		tagField = *watch.AlertTag
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
		fields := strings.Fields(line)

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
		if k > 0 && k <= len(fields) {
			if v, err := strconv.ParseFloat(fields[k-1], 64); err == nil {
				r.Value = &v
			}
		}
		if tagField > 0 && tagField <= len(fields) {
			r.Tag = fields[tagField-1]
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
func (ce *CronExecutor) tierForWatch(watch store.CronWatch) string {
	cust, err := ce.Store.GetCustomerByID(watch.CustomerID)
	if err != nil {
		return ""
	}
	sub, err := ce.Store.ActiveSubscription(cust.Name)
	if err != nil || sub == nil {
		return "trial"
	}
	return sub.Tier
}

// parseSchedule parses a schedule string into a duration.
// Delegates to store.ParseScheduleInterval.
func parseSchedule(schedule string) time.Duration {
	return store.ParseScheduleInterval(schedule)
}
