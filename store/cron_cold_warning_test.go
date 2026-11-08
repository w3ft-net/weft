package store

import (
	"errors"
	"strings"
	"testing"
)

// A cron fires over a window as long as its schedule, so one that looks back
// nearly as far as recent data reaches can touch older, archived data. That
// counts against the monthly cold history budget, and spending the budget
// disables the cron. The save says so, without blocking: the save succeeded.
func TestSaveCronWatch_WarnsWhenTheWindowReachesOlderData(t *testing.T) {
	db := testOpen(t)
	org := mustOrg(t, db, "cron-cold-warning")

	save := func(name, schedule string) []string {
		t.Helper()
		err := db.SaveCronWatch(org, "", name, schedule, "cat vimgym.log | wc -l", true, nil)
		blocking, warnings := Split(err)
		if len(blocking) > 0 {
			t.Fatalf("%s: save rejected: %v", schedule, errors.Join(blocking...))
		}
		return warnings
	}
	mentionsBudget := func(ws []string) bool {
		for _, w := range ws {
			if strings.Contains(w, "cold history budget") {
				return true
			}
		}
		return false
	}

	for _, schedule := range []string{"every 5m", "0 * * * *", "0 0 * * *", "every 7d", "every 12d"} {
		if ws := save("short-"+strings.ReplaceAll(schedule, " ", "-"), schedule); mentionsBudget(ws) {
			t.Errorf("%q warned about the cold budget: %v", schedule, ws)
		}
	}
	for _, schedule := range []string{"every 13d", "every 14d", "every 3w"} {
		ws := save("long-"+strings.ReplaceAll(schedule, " ", "-"), schedule)
		if !mentionsBudget(ws) {
			t.Fatalf("%q did not warn about the cold budget: %v", schedule, ws)
		}
		for _, w := range ws {
			for _, banned := range []string{"shard", "lambda", "parquet"} {
				if strings.Contains(strings.ToLower(w), banned) {
					t.Errorf("warning uses internal vocabulary %q: %s", banned, w)
				}
			}
		}
	}
}
