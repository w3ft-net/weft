package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCompileWatchPipeline_TextType(t *testing.T) {
	cases := []struct {
		name, pipeline, wantErr string
		wantType                string
	}{
		{"default is number", "echo 1", "", ValueNumber},
		{"text", "# type: text\necho i-0abc lease", "", ValueText},
		{"text with JSON value", "# type: text\n# value: .holder\n# tag: .lease\ncat x", "", ValueText},
		{"unknown type", "# type: number\necho 1", "'# type:' takes text", ""},
		{"type twice", "# type: text\n# type: text\necho a", "given twice", ""},
		{"text alert changed", "# type: text\necho a | alert changed", "", ValueText},
		{"text alert absent", "# type: text\necho a | alert absent", "", ValueText},
		{"text alert above", "# type: text\necho a | alert above 5", "this cron stores text (# type: text), and alert above compares numbers", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CompileWatchPipeline(c.pipeline)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got.ValueType != c.wantType {
				t.Fatalf("type=%q err=%v, want %q", got.ValueType, err, c.wantType)
			}
		})
	}
}

// Only spelled-out names are seen; a name built at run time is the runtime
// check's job.
func TestCompileWatchPipeline_NumericRefs(t *testing.T) {
	got, err := CompileWatchPipeline(`x=$(weft::avg errors 1h); weft::latest holders; weft::is_above 'disk' 80; weft::max $name 1h`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.NumericRefs) != "[errors disk]" {
		t.Fatalf("NumericRefs = %v, want [errors disk]", got.NumericRefs)
	}
}

func TestSaveCronWatch_TextTypeChecks(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("cron-types")
	save := func(name, pipeline string) error {
		return db.SaveCronWatch(org.ID, "", name, "every 5m", pipeline, true, nil)
	}
	if err := save("holders", "# type: text\necho i-0abc"); err != nil {
		t.Fatal(err)
	}
	if err := save("errors", "echo 1"); err != nil {
		t.Fatal(err)
	}

	// A numeric reader of a text cron is refused; a text reader is fine.
	if err := save("avg-holders", "weft::avg holders 1h"); err == nil || !strings.Contains(err.Error(), `cron "holders" stores text`) {
		t.Fatalf("numeric read of a text cron: %v", err)
	}
	if err := save("latest-holders", "weft::latest holders"); err != nil {
		t.Fatalf("weft::latest of a text cron: %v", err)
	}

	// A cron can't turn text while numeric readers name it, and the
	// refusal lists them.
	if err := save("errors-trend", "weft::slope errors 6h | alert above 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDashboard(org.ID, "", "ops", "tiles:\n  - kind: sparkline\n    cron: errors\n  - kind: table\n    cron: errors\n"); err != nil {
		t.Fatal(err)
	}
	err := save("errors", "# type: text\necho busy")
	if err == nil || !strings.Contains(err.Error(), "/cron.d/errors-trend") || !strings.Contains(err.Error(), "/etc/dashboards/ops.yaml (sparkline)") {
		t.Fatalf("turning a read cron text: %v", err)
	}

	// A numeric tile on a text cron is refused at dashboard save; a table
	// takes it.
	if err := db.SaveDashboard(org.ID, "", "leases", "tiles:\n  - kind: counter\n    cron: holders\n"); err == nil || !strings.Contains(err.Error(), "a counter tile draws numbers") {
		t.Fatalf("counter on a text cron: %v", err)
	}
	if err := db.SaveDashboard(org.ID, "", "leases", "tiles:\n  - kind: table\n    cron: holders\n"); err != nil {
		t.Fatalf("table on a text cron: %v", err)
	}
}

func TestLatestCronRun_TableOrder(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("cron-latest")
	if err := db.SaveCronWatch(org.ID, "", "disk", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	w, _ := db.GetCronWatch(org.ID, "disk")
	v := func(f float64) *float64 { return &f }
	now := time.Now().Truncate(time.Second)
	if err := db.SaveCronResults([]CronResult{
		{CronWatchID: w.ID, RanAt: now.Add(-5 * time.Minute), Value: v(99), Tag: "old"},
		{CronWatchID: w.ID, RanAt: now, Value: v(40), Tag: "b"},
		{CronWatchID: w.ID, RanAt: now, Value: v(80), Tag: "a"},
		{CronWatchID: w.ID, RanAt: now.Add(time.Minute), Stderr: "timed out"}, // a failed run's diagnostic
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.LatestCronRun(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Tag != "a" || rows[1].Tag != "b" {
		t.Fatalf("latest = %+v, want a (80) then b (40) from the newest run with points", rows)
	}
}
