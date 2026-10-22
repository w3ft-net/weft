package coordinator

import (
	"bufio"
	"context"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
)

const (
	// cronRunTimeout is the budget for one run of a cron.
	cronRunTimeout = 2 * time.Second
	// cronBenchStrikes is how many runs in a row may blow the budget before
	// the cron is disabled.
	cronBenchStrikes = 3
)

// runOutcome carries facts about one run back from deep inside it. The alert
// stage is built inside ExecuteForWatch, out of the executor's reach, so it
// reports through the run's context.
type runOutcome struct{ deliveryTimedOut atomic.Bool }

type runOutcomeKey struct{}

func withRunOutcome(ctx context.Context) (context.Context, *runOutcome) {
	o := &runOutcome{}
	return context.WithValue(ctx, runOutcomeKey{}, o), o
}

// noteDeliveryTimeout records that a notification target timed out during the
// run in ctx; outside a cron run it does nothing.
func noteDeliveryTimeout(ctx context.Context) {
	if o, ok := ctx.Value(runOutcomeKey{}).(*runOutcome); ok {
		o.deliveryTimedOut.Store(true)
	}
}

// CronExecutor runs scheduled cron watches in the background.
// It follows the same goroutine-per-task pattern as the poller.
type CronExecutor struct {
	Store    *store.Store
	Coord    *Coordinator
	Notifier *Notifier
	// Holder identifies this coordinator process: it is recorded on every
	// slot claim and names the owner of the overdue-guard lease.
	Holder string
	// Execute fires one run of a watch over tr. Nil means executeWatch;
	// tests substitute it so scheduling can be exercised without a
	// Coordinator.
	Execute func(ctx context.Context, w store.CronWatch, tr TimeRange)
	// Claims is the slot-claim backend. Nil means Store; tests substitute it
	// for fault injection.
	Claims slotClaimer
	// RunTimeout is the budget for one run. Zero means cronRunTimeout;
	// tests shrink it.
	RunTimeout time.Duration
	// SendSystem delivers an overdue page. Nil means Notifier.Send;
	// tests substitute it.
	SendSystem func(ctx context.Context, orgID store.OrgID, alias, watchName, body string) error

	mu      sync.Mutex
	cancels map[uint]context.CancelFunc // watchID → cancel
}

// Start begins the cron executor. It loads watches from the database,
// spawns a goroutine for each enabled watch, and reconciles every 30 seconds.
func (ce *CronExecutor) Start(ctx context.Context) {
	ce.cancels = make(map[uint]context.CancelFunc)

	// Initial load.
	ce.reconcile(ctx)
	go ce.runOverdueGuard(ctx)
	weftsdk.Infof(ctx, "cron: started (%d watches)", len(ce.cancels))

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
				ce.pruneClaims(ctx)
			}
		}
	}()
}

// reconcile syncs running goroutines with the database state: every enabled
// cron runs under per-slot claims (runClaims), whether or not it is new.
func (ce *CronExecutor) reconcile(ctx context.Context) {
	var watches []store.CronWatch
	err := ce.Store.DB().WithContext(store.WithoutTenantScope(ctx)).Where("enabled = ?", true).Find(&watches).Error
	if err != nil {
		weftsdk.Errorf(ctx, "cron: failed to list watches: %v", err)
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
			go ce.runClaims(watchCtx, w)
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

// intervalBelowMin reports whether interval is under the minimum for the
// watch's tier, with the tier and minimum for the message. Saves are
// validated, so this is belt-and-suspenders.
func (ce *CronExecutor) intervalBelowMin(watch store.CronWatch, interval time.Duration) (tier store.Tier, min time.Duration, below bool) {
	tier = ce.tierForWatch(watch)
	if tier == "" {
		return tier, 0, false
	}
	min = store.MinWatchInterval(tier)
	return tier, min, min > 0 && interval < min
}

// intervalAllowed reports whether interval meets the minimum for the watch's
// tier, logging when it does not. It is checked before any lease or claim is
// taken, since there is no point squatting on a watch that will never run.
func (ce *CronExecutor) intervalAllowed(ctx context.Context, watch store.CronWatch, interval time.Duration) bool {
	if tier, min, below := ce.intervalBelowMin(watch, interval); below {
		weftsdk.Infof(ctx, "cron[%s]: skipping — interval %s is below minimum %s for tier %s",
			watch.Name, interval, min, tier)
		return false
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

func (ce *CronExecutor) fire(ctx context.Context, watch store.CronWatch, tr TimeRange) {
	if ce.Execute != nil {
		ce.Execute(ctx, watch, tr)
		return
	}
	ce.executeWatch(ctx, watch, tr)
}

// executeWatch runs the watch's pipeline and records the result.
func (ce *CronExecutor) executeWatch(ctx context.Context, watch store.CronWatch, tr TimeRange) {
	start := time.Now()

	pipeline := ce.resolvePipeline(watch)

	// ExecuteForWatch runs the query and (if the pipeline ends with `| alert`)
	// evaluates the alert in the watch's AlertContext — incident dedup and
	// notifier dispatch both key off the watch identity instead of an
	// interactive-mode synthetic one.
	// ctx carries the trace ID minted when the slot claim was won (see
	// claimTick), so everything this firing logs or calls out to shares it.
	budget := ce.RunTimeout
	if budget == 0 {
		budget = cronRunTimeout
	}
	if d, err := ce.Store.CronRunTimeout(watch.OrgID); err != nil {
		weftsdk.Errorf(ctx, "cron[%s]: reading the org's run budget: %v", watch.Name, err)
	} else if d > 0 {
		budget = d
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	runCtx, outcome := withRunOutcome(runCtx)
	result, err := ce.Coord.ExecuteForWatch(runCtx, pipeline, tr, &watch)

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
		drained, drainErr := result.Drain(runCtx)
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

	// A run that failed with its deadline spent timed out, whatever error
	// the cancellation surfaced as.
	timedOut := status == "error" && runCtx.Err() == context.DeadlineExceeded
	if timedOut {
		status = "timeout"
		stderr = fmt.Sprintf("timed out after %s", budget)
	}

	// Persist result(s). Single-row mode: one cron_results row with
	// tag='' and value auto-parsed from stdout. Multi-row mode (watch has
	// alert_key set): split stdout into lines, extract value from field
	// alert_key and tag from alert_tag, one row per line. Lines whose -v
	// field isn't numeric are persisted with Value=NULL so the debugging
	// trail stays intact; aggregates ignore NULLs.
	if saveErr := ce.persistResults(watch, start, stdout, stderr, exitCode, duration); saveErr != nil {
		weftsdk.Errorf(ctx, "cron[%s]: failed to save result: %v", watch.Name, saveErr)
	}

	// Update watch status. A tick is a strike when the run blew its budget or
	// a notification target timed out; any other tick clears the strikes, and
	// the last strike disables the cron.
	strike := timedOut || outcome.deliveryTimedOut.Load()
	now := time.Now()
	updates := map[string]any{"last_run_at": &now, "last_status": status}
	if !strike {
		updates["consecutive_timeouts"] = 0
	}
	if updateErr := ce.Store.DB().WithContext(store.WithoutTenantScope(ctx)).Model(&store.CronWatch{}).Where("id = ?", watch.ID).Updates(updates).Error; updateErr != nil {
		weftsdk.Errorf(ctx, "cron[%s]: failed to update run status: %v", watch.Name, updateErr)
	}
	if strike {
		benched, benchErr := ce.Store.RecordCronTimeout(ctx, watch.ID, cronBenchStrikes)
		switch {
		case benchErr != nil:
			weftsdk.Errorf(ctx, "cron[%s]: failed to record timeout: %v", watch.Name, benchErr)
		case benched:
			weftsdk.Warnf(ctx, "cron[%s]: disabled after %d timeouts in a row (run budget %s); re-enable it by saving /cron.d/%s without '# enabled: false'",
				watch.Name, cronBenchStrikes, budget, watch.Name)
			why := fmt.Sprintf("its run exceeded the %s budget", budget)
			if !timedOut {
				why = "a notification target did not accept its alert in time"
			}
			ce.pageSystem(ctx, watch.OrgID, "cron-benched", fmt.Sprintf(
				"cron %q was disabled after %d timeouts in a row: %s.\nIt will not run again until you re-enable it: edit /cron.d/%s, remove the '# enabled: false' line, and save.",
				watch.Name, cronBenchStrikes, why, watch.Name))
		}
	}

	weftsdk.Infof(ctx, "cron[%s]: %s (%dms, stdout=%d bytes, stderr=%d bytes)",
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
