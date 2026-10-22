package coordinator

import (
	"context"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"sort"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/store"
)

const (
	// cronOverdueFactor is how many intervals a cron may go without a run
	// before it is overdue.
	cronOverdueFactor = 2
	// cronOverduePoll is how often the guard checks.
	cronOverduePoll = time.Minute
	// cronOverdueLease is the lease that makes exactly one coordinator run
	// the guard, so a stopped cron pages once and not once per instance.
	cronOverdueLease = "cron-overdue-guard"
)

// overdueCron is an enabled cron that has not run for more than
// cronOverdueFactor intervals.
type overdueCron struct {
	watch    store.CronWatch
	interval time.Duration
	age      time.Duration // since its last run (or creation, if it never ran)
}

// overdueCrons returns the watches in watches that are overdue at now, in
// watch ID order. The heartbeat is cron_watches.last_run_at, which executeWatch
// writes at the end of every run in either scheduling mode. It is deliberately
// not cron_results: a cron with no output (a quiet alert pipeline) writes no
// result rows, so its results look dead while it runs fine. A cron that has
// never run counts from created_at.
func overdueCrons(watches []store.CronWatch, now time.Time) []overdueCron {
	var out []overdueCron
	for _, w := range watches {
		if !w.Enabled {
			continue
		}
		interval := parseSchedule(w.Schedule)
		if interval <= 0 {
			continue
		}
		last := w.CreatedAt
		if w.LastRunAt != nil {
			last = *w.LastRunAt
		}
		if age := now.Sub(last); age > cronOverdueFactor*interval {
			out = append(out, overdueCron{watch: w, interval: interval, age: age})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].watch.ID < out[j].watch.ID })
	return out
}

// overdueGuard is the guard's state between ticks.
type overdueGuard struct {
	// leaseCtx is non-nil while this process holds the guard lease; it is
	// cancelled if the lease is lost.
	leaseCtx context.Context
	// active holds the crons already reported overdue, so each episode is
	// reported once.
	active map[uint]bool
}

// runOverdueGuard checks every cron once a minute, on whichever coordinator
// holds the guard lease. It runs in the coordinator process on purpose: a
// cron that watched for stopped crons would stop with them, which is exactly
// what happened to every cron at once during a database outage.
func (ce *CronExecutor) runOverdueGuard(ctx context.Context) {
	g := &overdueGuard{active: map[uint]bool{}}
	ticker := time.NewTicker(cronOverduePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ce.overdueStep(ctx, g, time.Now())
		}
	}
}

// overdueStep takes or keeps the guard lease and, if held, runs one check.
// Reports whether a check ran.
func (ce *CronExecutor) overdueStep(ctx context.Context, g *overdueGuard, now time.Time) bool {
	if g.leaseCtx == nil || g.leaseCtx.Err() != nil {
		g.leaseCtx = nil
		leaseCtx, ok := store.AcquireAndHold(ctx, ce.Store, cronOverdueLease, ce.Holder)
		if !ok {
			// Another coordinator is the guard. Forget what we reported,
			// so taking over later re-reports anything still overdue.
			clear(g.active)
			return false
		}
		g.leaseCtx = leaseCtx
	}
	ce.overdueTick(ctx, g, now)
	return true
}

// overdueTick reports crons that newly became overdue, and logs ones that
// recovered. A cron that stays overdue is not re-reported. Newly overdue crons
// are paged as one message per org, so an outage that stops every cron at once
// is one page per tenant and not one per cron.
func (ce *CronExecutor) overdueTick(ctx context.Context, g *overdueGuard, now time.Time) {
	var watches []store.CronWatch
	err := ce.Store.DB().WithContext(store.WithoutTenantScope(ctx)).Where("enabled = ?", true).Find(&watches).Error
	if err != nil {
		weftsdk.Errorf(ctx, "cron: overdue check failed to list watches: %v", err)
		return
	}

	seen := map[uint]bool{}
	total := map[store.OrgID]int{}           // overdue now, per org
	fresh := map[store.OrgID][]overdueCron{} // newly overdue this tick, per org
	for _, o := range overdueCrons(watches, now) {
		// A cron that cannot run at its tier's minimum interval is never
		// going to; it would be overdue forever.
		if _, _, below := ce.intervalBelowMin(o.watch, o.interval); below {
			continue
		}
		seen[o.watch.ID] = true
		total[o.watch.OrgID]++
		if g.active[o.watch.ID] {
			continue
		}
		g.active[o.watch.ID] = true
		weftsdk.Infof(ctx, "cron_overdue name=%s id=%d interval=%s age=%s", o.watch.Name, o.watch.ID, o.interval, o.age.Round(time.Second))
		fresh[o.watch.OrgID] = append(fresh[o.watch.OrgID], o)
	}
	for id := range g.active {
		if !seen[id] {
			delete(g.active, id)
			weftsdk.Infof(ctx, "cron_overdue_cleared id=%d", id)
		}
	}

	orgs := make([]store.OrgID, 0, len(fresh))
	for org := range fresh {
		orgs = append(orgs, org)
	}
	sort.Slice(orgs, func(i, j int) bool { return orgs[i] < orgs[j] })
	for _, org := range orgs {
		ce.notifyOverdue(ctx, org, fresh[org], total[org])
	}
}

// notifyOverdue pages one org about the crons that newly went overdue, through
// the org's reserved `system` alias in its notify.yaml. An org that has not
// defined it gets nothing; that is expected, so the failure is only logged.
func (ce *CronExecutor) notifyOverdue(ctx context.Context, org store.OrgID, crons []overdueCron, totalOverdue int) {
	var b strings.Builder
	fmt.Fprintf(&b, "%d cron(s) have not run for twice their interval:\n", len(crons))
	for _, o := range crons {
		fmt.Fprintf(&b, "  - %s: last ran %s ago (runs every %s)\n", o.watch.Name, o.age.Round(time.Second), o.interval)
	}
	if totalOverdue > len(crons) {
		fmt.Fprintf(&b, "%d overdue in total.\n", totalOverdue)
	}

	ce.pageSystem(ctx, org, "cron-overdue", b.String())
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
