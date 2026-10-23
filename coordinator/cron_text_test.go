package coordinator

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

// A cron's bash can run Lua inline, and that Lua reads cron history through
// the weft module. Here an inline program averages another cron and the run
// stores the result; the same program aimed at a text cron is refused at
// save, and one that builds the name at run time is refused when it runs.
func TestCron_InlineLuaReadsCronHistory(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "errors", []string{"2", "4", "6"}, time.Minute)
	if err := db.SaveCronWatch(custID, "", "error-avg", "every 5m",
		`lua 'print(weft.avg("errors", "1h"))' | alert above 5`, true, nil); err != nil {
		t.Fatal(err)
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	ce := &CronExecutor{Store: db, Coord: c, RunTimeout: 5 * time.Second}
	run := func(name string) store.CronWatch {
		t.Helper()
		w, err := db.GetCronWatch(custID, name)
		if err != nil {
			t.Fatal(err)
		}
		ce.executeWatch(context.Background(), *w, TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})
		w, _ = db.GetCronWatch(custID, name)
		return *w
	}
	w := run("error-avg")
	if w.LastStatus != "ok" {
		t.Fatalf("status = %q", w.LastStatus)
	}
	rows, err := db.LatestCronRun(w.ID)
	if err != nil || len(rows) != 1 || rows[0].Value == nil || *rows[0].Value != 4 {
		t.Fatalf("latest = %+v err=%v, want avg 4", rows, err)
	}

	if err := db.SaveCronWatch(custID, "", "holders", "every 5m", "# type: text\necho i-0abc", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "holder-avg", "every 5m",
		`lua 'print(weft.avg("holders", "1h"))'`, true, nil); err == nil || !strings.Contains(err.Error(), `cron "holders" stores text`) {
		t.Fatalf("inline Lua reading a text cron saved: %v", err)
	}
	if err := db.SaveCronWatch(custID, "", "holder-avg", "every 5m",
		`lua 'print(weft.avg("hol" .. "ders", "1h"))'`, true, nil); err != nil {
		t.Fatalf("a name built at run time can't be checked at save: %v", err)
	}
	if w := run("holder-avg"); w.LastStatus == "ok" {
		t.Fatalf("holder-avg status = %q, want the run-time check to fail it", w.LastStatus)
	}
	hw, _ := db.GetCronWatch(custID, "holder-avg")
	if res, _ := db.ListCronResults(hw.ID, 1, ""); len(res) != 1 || !strings.Contains(res[0].Stderr, "stores text") {
		t.Fatalf("holder-avg stored %+v, want the stores-text error", res)
	}
}

func TestParseCronOutput_Text(t *testing.T) {
	text := cronSchema{text: true}
	points, err := parseCronOutput(text, "i-0abc lease-a\ni-0def lease-b\n")
	if err != nil || len(points) != 2 || points[0].text != "i-0abc" || points[0].tag != "lease-a" {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	if _, err := parseCronOutput(text, "i-0abc lease-a extra\n"); err == nil || !strings.Contains(err.Error(), "has 3 fields") {
		t.Fatalf("three fields on a text cron: %v", err)
	}
	headers := cronSchema{text: true, value: mustSpec(t, ".holder"), tag: mustSpec(t, ".lease")}
	points, err = parseCronOutput(headers, `{"lease":"a","holder":"i-0abc"}`)
	if err != nil || len(points) != 1 || points[0].text != "i-0abc" || points[0].tag != "a" {
		t.Fatalf("JSON text points=%+v err=%v", points, err)
	}
	if _, err := parseCronOutput(headers, `{"lease":"a"}`); err == nil || !strings.Contains(err.Error(), "has no .holder") {
		t.Fatalf("missing text value: %v", err)
	}
}

// A text cron stores its values as text with no number, so the rollups
// never see them; numeric weft:: functions refuse it at run time when the
// save couldn't see the name.
func TestTextCron_StoresTextAndRefusesNumericReads(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveShellFunction(custID, "", "holders", `printf 'i-0abc lease-a\ni-0def lease-b\n'`, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "holders", "every 5m", "# type: text\nholders", true, nil); err != nil {
		t.Fatal(err)
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	ce := &CronExecutor{Store: db, Coord: c, RunTimeout: 5 * time.Second}
	w, _ := db.GetCronWatch(custID, "holders")
	ce.executeWatch(context.Background(), *w, TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})

	w, _ = db.GetCronWatch(custID, "holders")
	if w.LastStatus != "ok" {
		t.Fatalf("status = %q", w.LastStatus)
	}
	rows, err := db.LatestCronRun(w.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("latest = %+v err=%v", rows, err)
	}
	for _, r := range rows {
		if r.Value != nil || r.TextValue == nil {
			t.Fatalf("text row %+v: want a text value and no number", r)
		}
	}
	if rows[0].Tag != "lease-a" || *rows[0].TextValue != "i-0abc" {
		t.Errorf("first row = %s=%s, want lease-a=i-0abc", rows[0].Tag, *rows[0].TextValue)
	}

	wf := &WeftFuncs{Store: db, OrgID: custID}
	if _, err := wf.Avg("holders", time.Hour); err == nil || !strings.Contains(err.Error(), `cron "holders" stores text`) {
		t.Fatalf("weft::avg of a text cron: %v", err)
	}
	if _, err := wf.Percentile("holders", time.Hour, 0.5); err == nil || !strings.Contains(err.Error(), "stores text") {
		t.Fatalf("weft::percentile of a text cron: %v", err)
	}
	if _, err := wf.Absent("holders", time.Hour); err != nil {
		t.Fatalf("weft::is_absent reads text crons: %v", err)
	}
	if got, err := wf.Latest("holders"); err != nil || got == "" {
		t.Fatalf("weft::latest = %q, %v", got, err)
	}
	if runs, err := wf.Describe("holders", 2); err != nil || len(runs) != 2 || runs[0].Text == nil || runs[0].Value != nil {
		t.Fatalf("weft::describe = %+v, %v; want text values and no number", runs, err)
	}

	// A bash predicate reports the mistake (exit 2) instead of answering
	// "can't tell" (exit 1), which would read as the condition not holding.
	var stderr strings.Builder
	above := numericPredicate(wf, "weft::is_above", predicateAbove(wf))
	if code := above([]string{"holders", "5"}, nil, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "stores text") {
		t.Fatalf("weft::is_above on a text cron: exit %d, stderr %q", code, stderr.String())
	}
}
