package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/coordination"
	"github.com/graybeardcorp/weft/store"
)

const (
	// cronRunTimeout is the budget for one run of a cron.
	cronRunTimeout = 2 * time.Second
	// cronBenchStrikes is how many runs in a row may run out of time (the
	// run budget, a delivery timeout) before the cron is disabled.
	cronBenchStrikes = 3
)

// runOutcome carries facts about one run back from deep inside it. The alert
// stage is built inside ExecuteForWatch, out of the executor's reach, so it
// reports through the run's context.
type runOutcome struct {
	deliveryTimedOut atomic.Bool
	// typeError is set when the run tripped a check that a save would have
	// made had it seen the name (a numeric read of a text cron). Like output
	// that breaks the contract, it disables the cron at once.
	typeError atomic.Pointer[string]
}

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

// noteTypeError records that the run in ctx failed a check a save would
// have made; outside a cron run it does nothing. The first one wins.
func noteTypeError(ctx context.Context, err error) {
	if o, ok := ctx.Value(runOutcomeKey{}).(*runOutcome); ok {
		msg := err.Error()
		o.typeError.CompareAndSwap(nil, &msg)
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
	Claims coordination.Claims
	// RunTimeout is the budget for one run. Zero means cronRunTimeout;
	// tests shrink it.
	RunTimeout time.Duration
	// SendSystem delivers a system-alias page. Nil means Notifier.Send;
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

	// budgetErr is set when the run needed cold history after the org's month
	// of cold scanning was spent; it disables the cron below.
	var budgetErr *store.ColdScanBudgetError
	if err != nil {
		status = "error"
		stderr = err.Error()
		exitCode = 1
		errors.As(err, &budgetErr)
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
			errors.As(drainErr, &budgetErr)
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

	// Split the output into points under the cron's output contract. Output
	// that breaks it stores none of its points and disables the cron.
	var points []cronPoint
	var invalid error
	if status != "error" && status != "timeout" {
		points, invalid = ce.checkOutput(watch, stdout)
		if invalid != nil {
			status = "invalid"
			stderr = strings.TrimSpace(invalid.Error() + "\n" + stderr)
			exitCode = 1
		}
	}
	if saveErr := ce.persistResults(watch, start, points, stderr, exitCode, duration); saveErr != nil {
		weftsdk.Errorf(ctx, "cron[%s]: failed to save result: %v", watch.Name, saveErr)
	}

	// Update watch status. A run whose output's shape is wrong is disabled at
	// once: output that broke the contract or the plan's tag cap, or a check
	// a save would have made (a numeric read of a text cron, through a name
	// only known at run time). Time is struck instead: a tick that blew its
	// budget or whose notification target timed out is a strike, any other
	// tick clears the strikes, and the last strike disables the cron.
	var disableWhy, disableHint string
	if budgetErr != nil {
		// A refusal repeats until the month resets, so further fires would
		// only burn runs: disable at once rather than after strikes.
		status = "error"
		disableWhy = budgetErr.Error()
		disableHint = "Its time range reached cold history. Narrow the range to recent data, or wait for the budget to reset."
	} else if typeErr := outcome.typeError.Load(); typeErr != nil {
		status = "error"
		disableWhy = *typeErr
		disableHint = "Saving it would have been refused had the cron's name been spelled out."
	} else if invalid != nil {
		disableWhy = invalid.Error()
		disableHint = "Its output didn't match what the cron declares, so none of it was stored."
	}
	var why string
	switch {
	case disableWhy != "":
	case timedOut:
		why = fmt.Sprintf("its run exceeded the %s budget", budget)
	case outcome.deliveryTimedOut.Load():
		why = "a notification target did not accept its alert in time"
	}
	strike := why != ""
	now := time.Now()
	updates := map[string]any{"last_run_at": &now, "last_status": status}
	if !strike {
		updates["consecutive_strikes"] = 0
	}
	if updateErr := ce.Store.DB().WithContext(store.WithoutTenantScope(ctx)).Model(&store.CronWatch{}).Where("id = ?", watch.ID).Updates(updates).Error; updateErr != nil {
		weftsdk.Errorf(ctx, "cron[%s]: failed to update run status: %v", watch.Name, updateErr)
	}
	if disableWhy != "" {
		disabled, err := ce.Store.DisableCron(ctx, watch.ID)
		switch {
		case err != nil:
			weftsdk.Errorf(ctx, "cron[%s]: failed to disable: %v", watch.Name, err)
		case disabled:
			weftsdk.Warnf(ctx, "cron[%s]: disabled: %s", watch.Name, disableWhy)
			ce.pageSystem(ctx, watch.OrgID, "cron disabled", fmt.Sprintf(
				"cron %q was disabled. Cause: %s.\n%s After fixing, re-enable by editing /cron.d/%s to remove the '# enabled: false' line.",
				watch.Name, disableWhy, disableHint, watch.Name))
		}
	}
	if strike {
		benched, benchErr := ce.Store.RecordCronStrike(ctx, watch.ID, cronBenchStrikes)
		switch {
		case benchErr != nil:
			weftsdk.Errorf(ctx, "cron[%s]: failed to record strike: %v", watch.Name, benchErr)
		case benched:
			weftsdk.Warnf(ctx, "cron[%s]: disabled after %d strikes in a row (last: %s); re-enable it by saving /cron.d/%s without '# enabled: false'",
				watch.Name, cronBenchStrikes, why, watch.Name)
			ce.pageSystem(ctx, watch.OrgID, "cron disabled", fmt.Sprintf(
				"cron %q disabled after %d consecutive failed runs. Most recent failure: %s.\nAfter fixing, reenable by editing /cron.d/%s to remove the '# enabled: false' line.",
				watch.Name, cronBenchStrikes, why, watch.Name))
		}
	}

	weftsdk.Infof(ctx, "cron[%s]: %s (%dms, stdout=%d bytes, stderr=%d bytes)",
		watch.Name, status, duration.Milliseconds(), len(stdout), len(stderr))
}

// pageSystem sends body to an org's reserved `system` alias, for the alerts
// weft raises on the tenant's behalf. An org that has not defined the alias
// gets nothing; that is expected, so a failure is only logged.
func (ce *CronExecutor) pageSystem(ctx context.Context, org store.OrgID, watchName, body string) {
	send := ce.SendSystem
	if send == nil && ce.Notifier != nil {
		send = func(ctx context.Context, org store.OrgID, alias, watchName, body string) error {
			_, err := ce.Notifier.Send(ctx, org, alias, watchName, body, nil)
			return err
		}
	}
	if send == nil {
		return
	}
	if err := send(ctx, org, store.SystemAlias, watchName, body); err != nil {
		weftsdk.Infof(ctx, "%s not delivered for org %d: %v", strings.ReplaceAll(watchName, "-", "_"), org, err)
	}
}

// checkOutput splits a run's stdout into points and applies the runtime
// checks: the output contract, then the plan's tag cap. A non-nil error says
// why the cron is disabled.
func (ce *CronExecutor) checkOutput(watch store.CronWatch, stdout string) ([]cronPoint, error) {
	schema, err := schemaForWatch(watch)
	if err != nil {
		return nil, err
	}
	points, err := parseCronOutput(schema, stdout)
	if err != nil {
		return nil, err
	}
	for _, p := range points {
		if p.tag != "" {
			return points, checkTagCap(points, store.TagsPerRun(ce.tierForWatch(watch)))
		}
	}
	return points, nil
}

// persistResults writes this tick's cron_results rows: one per point, with
// stderr and exit code on the first (they describe the whole tick, not a
// point). A tick with no points stores nothing unless it has something to
// say, a failure or an alert firing, which goes on one untagged row with no
// value so the rollups never see it.
func (ce *CronExecutor) persistResults(watch store.CronWatch, ranAt time.Time, points []cronPoint, stderr string, exitCode int, duration time.Duration) error {
	if len(points) == 0 {
		if stderr == "" && exitCode == 0 {
			return nil
		}
		return ce.Store.SaveCronResults([]store.CronResult{{
			CronWatchID: watch.ID,
			RanAt:       ranAt,
			Stderr:      stderr,
			ExitCode:    exitCode,
			DurationMs:  duration.Milliseconds(),
		}})
	}
	text := watch.ValueType == store.ValueText
	rows := make([]store.CronResult, len(points))
	for i, p := range points {
		rows[i] = store.CronResult{
			CronWatchID: watch.ID,
			RanAt:       ranAt,
			Stdout:      p.line,
			ExitCode:    exitCode,
			DurationMs:  duration.Milliseconds(),
			Tag:         p.tag,
		}
		if text {
			t := p.text
			rows[i].TextValue = &t
		} else {
			v := p.value
			rows[i].Value = &v
		}
	}
	rows[0].Stderr = stderr
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
