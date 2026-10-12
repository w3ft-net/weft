package coordinator

import (
	"io"
	"testing"
	"time"
)

// is_absent means "no result": a cron that doesn't exist, one that has never
// run, and one whose results are all older than the window are all absent,
// and every form agrees. The windowed form used to treat a missing cron as
// "can't tell" (exit 1, no fire) while the plain form fired, so
// `weft::is_absent canary-ping` and `weft::is_absent canary-ping 90s`
// disagreed about the same typo'd or deleted cron.
func TestWeftFuncs_IsAbsent_OneRuleForEveryForm(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "fresh", []string{"1"}, time.Minute)
	seedWatch(t, db, custID, "stale", []string{"1"}, time.Minute)
	if err := db.SaveCronWatch(custID, "", "never-ran", "every 1m", "echo test", nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, "stale")
	if err != nil {
		t.Fatal(err)
	}
	// Age the stale cron's only result out of any short window.
	if err := db.ExecForTest("UPDATE cron_results SET ran_at = ? WHERE cron_watch_id = ?", time.Now().Add(-48*time.Hour), w.ID); err != nil {
		t.Fatal(err)
	}
	wf := &WeftFuncs{Store: db, OrgID: custID}
	pred := predicateAbsent(wf)

	cases := []struct {
		name   string
		cron   string
		window string // "" = the plain form
		absent bool
	}{
		{"missing cron", "no-such-cron", "", true},
		{"missing cron, windowed", "no-such-cron", "90s", true},
		{"never ran", "never-ran", "", true},
		{"never ran, windowed", "never-ran", "90s", true},
		{"has a recent result", "fresh", "", false},
		{"has a recent result, windowed", "fresh", "10m", false},
		{"only an old result, windowed", "stale", "10m", true},
	}
	for _, c := range cases {
		args := []string{c.cron}
		if c.window != "" {
			args = append(args, c.window)
		}
		wantExit := 1
		if c.absent {
			wantExit = 0
		}
		if got := pred(args, nil, io.Discard, io.Discard); got != wantExit {
			t.Errorf("%s: weft::is_absent %v exit %d, want %d", c.name, args, got, wantExit)
		}
		var dur time.Duration
		if c.window != "" {
			dur, _ = time.ParseDuration(c.window)
		}
		got, err := wf.Absent(c.cron, dur)
		if err != nil || got != c.absent {
			t.Errorf("%s: Absent = %v, %v; want %v, nil", c.name, got, err, c.absent)
		}
	}

	// A window the history can't answer (over the cap) is "can't tell":
	// not absent, and an error — it must not page for a bad argument.
	if absent, err := wf.Absent("fresh", 365*24*time.Hour); err == nil || absent {
		t.Errorf("over-cap window: Absent = %v, %v; want false and an error", absent, err)
	}
}
