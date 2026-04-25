package coordinator

import (
	"strings"
	"testing"
	"time"
)

// TestCronShareScriptAcrossWatches — the motivating use case. One /bin
// script, two watches passing different args. Each QueryForWatch
// dispatches the script's body with the watch's arg bound, producing
// distinct output per watch. This is the path a real cron tick takes.
func TestCronShareScriptAcrossWatches(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveShellFunction(custID, "", "announce", `echo "source=$1"`); err != nil {
		t.Fatalf("save script: %v", err)
	}

	if err := db.SaveCronWatch(custID, "", "disk-check", "every 1m", "announce disk.log"); err != nil {
		t.Fatalf("save disk watch: %v", err)
	}
	if err := db.SaveCronWatch(custID, "", "cpu-check", "every 1m", "announce cpu.log"); err != nil {
		t.Fatalf("save cpu watch: %v", err)
	}

	diskW, err := db.GetCronWatch(custID, "disk-check")
	if err != nil {
		t.Fatal(err)
	}
	cpuW, err := db.GetCronWatch(custID, "cpu-check")
	if err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{Store: db}
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	diskResult, err := c.QueryForWatch(strings.TrimSpace(diskW.Pipeline), tr, diskW)
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	if got := strings.TrimSpace(diskResult.Text); got != "source=disk.log" {
		t.Errorf("disk: got %q, want source=disk.log (stderr=%q)", got, diskResult.Stderr)
	}

	cpuResult, err := c.QueryForWatch(strings.TrimSpace(cpuW.Pipeline), tr, cpuW)
	if err != nil {
		t.Fatalf("cpu: %v", err)
	}
	if got := strings.TrimSpace(cpuResult.Text); got != "source=cpu.log" {
		t.Errorf("cpu: got %q, want source=cpu.log (stderr=%q)", got, cpuResult.Stderr)
	}
}

// TestCronBareNameStillWorks — regression guard. The old cron.resolvePipeline
// special-cased bare-name pipelines. After simplification, bare names must
// still resolve through the coordinator's pre-expansion path.
func TestCronBareNameStillWorks(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveShellFunction(custID, "", "hello", `echo hello`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "greeter", "every 1m", "hello"); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, "greeter")
	if err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{Store: db}
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	result, err := c.QueryForWatch(strings.TrimSpace(w.Pipeline), tr, w)
	if err != nil {
		t.Fatalf("QueryForWatch: %v", err)
	}
	if got := strings.TrimSpace(result.Text); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}
