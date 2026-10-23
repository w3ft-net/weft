package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
)

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
}
