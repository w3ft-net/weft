package identity

import (
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// Limits is what a deployment holds one group to. Every field's zero value is
// no limit, so a build with no plans (the open one) runs on the zero Limits and
// the code that reads them never has to know whether there are any.
type Limits struct {
	// Plan names the group's plan. The core treats it as an opaque label: it is
	// stamped on a lowered plan when a cron is saved and handed back to
	// Entitlements.LimitsOfPlan when the cron fires.
	Plan string
	// Retention is how far back the group may read, in logs and in a cron's
	// history. Zero reads everything there is.
	Retention time.Duration
	// MaxCrons is how many enabled crons the group may have. Zero is no limit.
	MaxCrons int
	// MinCronInterval is the shortest schedule a cron may run on.
	MinCronInterval time.Duration
	// TagsPerRun is how many distinct tags one run of a cron may print.
	TagsPerRun int
	// ColdScanLines is the lines the group may scan from cold storage in a
	// calendar month (ColdScanPeriod). Nil is unmetered; a pointer to zero is a
	// group with no cold history to read.
	ColdScanLines *int64
}

// PlanLimitError is a write refused because the group's plan does not allow it
// (too many crons, a schedule shorter than the plan's minimum). It is not a
// permission problem: the caller may write here, the plan may not. So callers
// that map errors to statuses can tell the two apart.
type PlanLimitError struct{ Msg string }

func (e *PlanLimitError) Error() string { return e.Msg }

// Horizon is the earliest instant the group may read, the zero time when there
// is no retention limit.
func (l Limits) Horizon() time.Time {
	if l.Retention <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-l.Retention)
}

// Clamp pushes since forward to the horizon when it reaches back further, so a
// reader never sees rows older than the plan allows even though they may still
// exist. A zero since is returned as it is: it means no lower bound was asked
// for.
func (l Limits) Clamp(since time.Time) time.Time {
	if since.IsZero() {
		return since
	}
	if h := l.Horizon(); since.Before(h) {
		return h
	}
	return since
}

// Entitlements is where a deployment says what each group's limits are. The
// default build supplies none, and the core runs unlimited.
type Entitlements interface {
	// LimitsFor is the limits of the group the columns name, as they stand now.
	LimitsFor(owner shape.Added) Limits
	// LimitsOfPlan is the limits of a plan label as a Limits carried it earlier,
	// without asking about any group: a lowered plan caches its group's plan so
	// that firing it costs no lookup.
	LimitsOfPlan(plan string) Limits
	// CronRunTimeout is the group's own budget for one cron run, when it has
	// one: zero means the default applies. Asked once per fire, not per query.
	CronRunTimeout(owner shape.Added) time.Duration
}

// ErrColdScanBudgetExhausted is returned, wrapped in a *ColdScanBudgetError,
// when a query needs cold data and the month's scan budget is spent.
var ErrColdScanBudgetExhausted = errors.New("cold scan budget exhausted")

// ColdScanBudgetError says how much of the month's cold history budget was
// used and when it resets. It matches ErrColdScanBudgetExhausted.
type ColdScanBudgetError struct {
	Used, Allowance int64
	ResetsAt        time.Time
}

func (e *ColdScanBudgetError) Error() string {
	return fmt.Sprintf("this month's cold history budget is used (%s of %s lines); queries over recent data still work, and it resets on %s",
		FormatLines(e.Used), FormatLines(e.Allowance), e.ResetsAt.Format("2 January"))
}

func (e *ColdScanBudgetError) Is(target error) bool { return target == ErrColdScanBudgetExhausted }

// FormatLines renders a line count for people: 34.4 million, 1.63 billion.
func FormatLines(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2f billion", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f million", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ColdScanPeriod is the calendar month (UTC) containing t: the period a cold
// scan allowance is counted over.
func ColdScanPeriod(t time.Time) (start, end time.Time) {
	t = t.UTC()
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}
