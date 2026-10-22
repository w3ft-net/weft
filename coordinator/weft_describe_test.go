package coordinator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	luapkg "github.com/graybeardcorp/warp/pkg/lua"

	"github.com/graybeardcorp/weft/store"
)

// weft::describe is the only way to read how past runs went (exit code,
// duration, stderr), not just what they printed.
func TestWeftFuncs_Describe(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveCronWatch(custID, "", "canary", "every 1m", "echo test", nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, "canary")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	rows := []store.CronResult{
		{CronWatchID: w.ID, RanAt: now.Add(-2 * time.Minute), Stdout: "3\n", DurationMs: 40},
		{CronWatchID: w.ID, RanAt: now.Add(-time.Minute), Stdout: "", Stderr: "boom", ExitCode: 1, DurationMs: 12},
		{CronWatchID: w.ID, RanAt: now, Stdout: "host-a 7", Tag: "host-a", DurationMs: 55},
	}
	for i := range rows {
		if err := saveCronResult(db, &rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	wf := &WeftFuncs{Store: db, OrgID: custID}

	got, err := wf.Describe("canary", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d runs, want 2 (n limits the count)", len(got))
	}
	if got[0].Tag != "host-a" || got[0].DurationMs != 55 || !got[0].RanAt.Equal(now) {
		t.Errorf("newest run first: got %+v", got[0])
	}
	if got[1].ExitCode != 1 || got[1].Stderr != "boom" || got[1].Value != nil {
		t.Errorf("failed run: got %+v", got[1])
	}

	all, _ := wf.Describe("canary", 10)
	if len(all) != 3 || all[2].Value == nil || *all[2].Value != 3 {
		t.Errorf("oldest run should carry parsed value 3: %+v", all)
	}

	if _, err := wf.Describe("no-such-cron", 10); err == nil {
		t.Error("unknown cron should be an error")
	}
	if _, err := wf.Describe("canary", 0); err == nil {
		t.Error("n=0 should be an error")
	}
	if _, err := wf.Describe("canary", maxDescribeRuns+1); err == nil {
		t.Error("n over the cap should be an error")
	}

	// JSONL rendering: one object per run, snake_case keys.
	var buf bytes.Buffer
	if err := writeRunsJSONL(&buf, got); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %q", buf.String())
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &obj); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ran_at", "exit_code", "duration_ms", "tag", "value", "stdout", "stderr"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("missing key %q in %s", k, lines[1])
		}
	}
	if obj["value"] != nil || obj["stderr"] != "boom" {
		t.Errorf("unexpected values: %v", obj)
	}
}

func TestWeftLua_Describe(t *testing.T) {
	m := newMock()
	m.runs = map[string][]RunRecord{"canary": {
		{RanAt: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), ExitCode: 1, DurationMs: 12, Stderr: "boom"},
	}}
	var out, errb bytes.Buffer
	code := luapkg.RunScript(`
local runs = weft.describe("canary", 5)
print(#runs, runs[1].exit_code, runs[1].stderr, runs[1].duration_ms, runs[1].ran_at)
`, nil, strings.NewReader(""), &out, &errb, luapkg.WithExtension(buildWeftLuaExtension(m)))
	if code != 0 {
		t.Fatalf("lua failed: %s", errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "1\t1\tboom\t12\t2026-10-21T12:00:00Z" {
		t.Errorf("got %q", got)
	}
}
