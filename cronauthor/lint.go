package cronauthor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/cron/cronsched"
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/warning"
)

// ColdGrazeWindow is how long a cron's window must be before a fire can reach
// archived data: the 14-day recent-data window less a day for the lag between a
// shard's window ending and its data moving. A
// fixed plan fact, like the hot window in scripts/costmodel/tiers.yaml.
const ColdGrazeWindow = 13 * 24 * time.Hour

// coldGrazeWarning is the save-time notice for a cron whose window can reach
// older data, or "" when it cannot. The window of each fire is the schedule's
// interval.
func coldGrazeWarning(schedule string) string {
	interval, err := cronsched.ValidateSchedule(schedule)
	if err != nil || interval < ColdGrazeWindow {
		return ""
	}
	return fmt.Sprintf("This cron looks back %s at each fire, close to the 14 days of recent data, so a fire can reach older data. "+
		"Older data counts against your monthly cold history budget. Once that is used up, a fire that needs it fails, the cron is disabled, "+
		"and a notice goes to your `system` alias, so define one to be told. Shorten the schedule to stay within recent data.",
		formatWindow(interval))
}

// formatWindow renders a look-back in days, or weeks when it is whole weeks.
func formatWindow(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	if days%7 == 0 {
		return fmt.Sprintf("%d weeks", days/7)
	}
	return fmt.Sprintf("%d days", days)
}

// Lint runs every save-time, non-blocking check against a watch that just saved
// successfully, and joins their messages into one warning-only error (nil if
// there's nothing to say). loweredPlan may be nil (lowering failed at save time,
// e.g. an unsupported pipeline shape): the checks that depend on it just no-op
// then. This never blocks a save that already succeeded.
func Lint(schedule, pipeline string, loweredPlan []byte) error {
	// The schedule check needs no plan, so it runs even when lowering failed.
	var msgs []string
	if m := coldGrazeWarning(schedule); m != "" {
		msgs = append(msgs, m)
	}
	var lp planner.LoweredPlan
	if len(loweredPlan) > 0 && json.Unmarshal(loweredPlan, &lp) == nil {
		msgs = append(planner.LintTruncationRisk(&lp), msgs...)
		// A cron pays for a stage that runs post-retrieval on every fire, so
		// say so once, at save. A pipeline with nothing to gain stays silent,
		// as does one that calls a /bin script: its stages aren't analyzed.
		if len(lp.BinDeps) == 0 {
			if a, err := planner.Analyze(pipeline, "/", nil); err == nil && a.Barrier != 0 {
				msgs = append(msgs, "This cron runs on every fire, so a stage that runs post-retrieval is paid for each time.\n\n"+strings.TrimRight(a.Render(false), "\n"))
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	warnings := make([]error, len(msgs))
	for i, m := range msgs {
		warnings[i] = warning.New("%s", m)
	}
	return errors.Join(warnings...)
}
