package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

// TestCronShareScriptAcrossWatches — the motivating use case. One /bin
// script, two watches passing different args. Each ExecuteForWatch
// dispatches the script's body with the watch's arg bound, producing
// distinct output per watch. This is the path a real cron tick takes.
func TestCronShareScriptAcrossWatches(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveShellFunction(custID, "", "announce", `echo "source=$1"`, "bash"); err != nil {
		t.Fatalf("save script: %v", err)
	}

	if err := db.SaveCronWatch(custID, "", "disk-check", "every 1m", "announce disk.log", true, nil); err != nil {
		t.Fatalf("save disk watch: %v", err)
	}
	if err := db.SaveCronWatch(custID, "", "cpu-check", "every 1m", "announce cpu.log", true, nil); err != nil {
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

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	diskResult, err := c.ExecuteForWatch(context.Background(), strings.TrimSpace(diskW.Pipeline), tr, diskW)
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	diskResultD := drainExec(t, diskResult)
	if got := strings.TrimSpace(diskResultD.Text); got != "source=disk.log" {
		t.Errorf("disk: got %q, want source=disk.log (stderr=%q)", got, diskResultD.Stderr)
	}

	cpuResult, err := c.ExecuteForWatch(context.Background(), strings.TrimSpace(cpuW.Pipeline), tr, cpuW)
	if err != nil {
		t.Fatalf("cpu: %v", err)
	}
	cpuResultD := drainExec(t, cpuResult)
	if got := strings.TrimSpace(cpuResultD.Text); got != "source=cpu.log" {
		t.Errorf("cpu: got %q, want source=cpu.log (stderr=%q)", got, cpuResultD.Stderr)
	}
}

// TestCronBareNameStillWorks — regression guard. The old cron.resolvePipeline
// special-cased bare-name pipelines. After simplification, bare names must
// still resolve through the coordinator's pre-expansion path.
func TestCronBareNameStillWorks(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveShellFunction(custID, "", "hello", `echo hello`, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "greeter", "every 1m", "hello", true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, "greeter")
	if err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	result, err := c.ExecuteForWatch(context.Background(), strings.TrimSpace(w.Pipeline), tr, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	resultD := drainExec(t, result)
	if got := strings.TrimSpace(resultD.Text); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

// TestCronBenchedAfterRepeatedTimeouts — a cron that keeps blowing its run
// budget is disabled; one run inside the budget resets the count.
func TestCronBenchedAfterRepeatedTimeouts(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveShellFunction(custID, "", "slowpoke", `while true; do :; done`, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(custID, "", "quick", `echo 1`, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "slow", "every 5m", "slowpoke", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "fast", "every 5m", "quick", true, nil); err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	var pages []string
	ce := &CronExecutor{Store: db, Coord: c, RunTimeout: 100 * time.Millisecond,
		SendSystem: func(_ context.Context, org store.OrgID, alias, _, body string) error {
			if org != custID || alias != store.SystemAlias {
				t.Errorf("page went to org %d alias %q, want org %d alias %q", org, alias, custID, store.SystemAlias)
			}
			pages = append(pages, body)
			return nil
		}}
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	run := func(name string) store.CronWatch {
		t.Helper()
		w, err := db.GetCronWatch(custID, name)
		if err != nil {
			t.Fatal(err)
		}
		ce.executeWatch(context.Background(), *w, tr)
		w, _ = db.GetCronWatch(custID, name)
		return *w
	}

	for i := 1; i <= cronBenchStrikes; i++ {
		w := run("slow")
		if w.LastStatus != "timeout" {
			t.Fatalf("run %d: last_status = %q, want timeout", i, w.LastStatus)
		}
		if w.Enabled != (i < cronBenchStrikes) {
			t.Fatalf("run %d: enabled = %v", i, w.Enabled)
		}
		if want := i / cronBenchStrikes; len(pages) != want {
			t.Fatalf("after run %d: %d page(s), want %d (only the benching run pages)", i, len(pages), want)
		}
	}
	if !strings.Contains(pages[0], "slow") || !strings.Contains(pages[0], "# enabled: false") {
		t.Errorf("page should name the cron and say how to re-enable it:\n%s", pages[0])
	}

	if w := run("fast"); w.LastStatus != "ok" || !w.Enabled || w.ConsecutiveStrikes != 0 {
		t.Fatalf("fast cron: status=%q enabled=%v strikes=%d", w.LastStatus, w.Enabled, w.ConsecutiveStrikes)
	}
}

// TestCronBenchedAfterRepeatedDeliveryTimeouts — the strike rule isn't about
// the pipeline alone: a cron whose notification target keeps timing out is
// benched too, and a tick whose deliveries go through clears the count.
func TestCronBenchedAfterRepeatedDeliveryTimeouts(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	hang := make(chan struct{})
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer dead.Close()
	defer close(hang)
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer live.Close()

	setHook := func(url string) {
		t.Helper()
		yaml := fmt.Sprintf("system:\n  email:\n    - oncall@example.test\nhook:\n  webhook:\n    - %q\n", url)
		if err := db.SaveNotifyAliases(custID, "", yaml); err != nil {
			t.Fatal(err)
		}
	}
	setHook(dead.URL)
	if err := db.SaveCronWatch(custID, "", "deaf-hook", "every 5m", "echo 1 | alert above 0 --notify=hook", true, nil); err != nil {
		t.Fatal(err)
	}

	n := NewNotifier(db, nil)
	n.Timeout = 100 * time.Millisecond
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db, Notifier: n})
	var pages int
	ce := &CronExecutor{Store: db, Coord: c, Notifier: n, RunTimeout: 5 * time.Second,
		SendSystem: func(context.Context, store.OrgID, string, string, string) error { pages++; return nil }}
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	run := func() store.CronWatch {
		t.Helper()
		w, err := db.GetCronWatch(custID, "deaf-hook")
		if err != nil {
			t.Fatal(err)
		}
		ce.executeWatch(context.Background(), *w, tr)
		w, _ = db.GetCronWatch(custID, "deaf-hook")
		return *w
	}

	run()
	if w := run(); w.ConsecutiveStrikes != 2 || !w.Enabled {
		t.Fatalf("after two dead ticks: strikes=%d enabled=%v, want 2/true", w.ConsecutiveStrikes, w.Enabled)
	}
	setHook(live.URL)
	if w := run(); w.ConsecutiveStrikes != 0 || !w.Enabled {
		t.Fatalf("after a delivered tick: strikes=%d enabled=%v, want 0/true", w.ConsecutiveStrikes, w.Enabled)
	}

	// The endpoint goes dead again; the incident is still open, and each
	// tick retries the target that missed it.
	setHook(dead.URL)
	var w store.CronWatch
	for i := 0; i < cronBenchStrikes; i++ {
		w = run()
	}
	if w.Enabled {
		t.Fatalf("still enabled after %d delivery timeouts in a row (strikes=%d)", cronBenchStrikes, w.ConsecutiveStrikes)
	}
	if pages != 1 {
		t.Errorf("%d page(s) for the benched cron, want 1", pages)
	}
}

func TestNotifierDeliverTimesOutAnyTarget(t *testing.T) {
	n := NewNotifier(nil, nil)
	n.Timeout = 50 * time.Millisecond
	err := n.deliver(context.Background(), func(ctx context.Context) error {
		time.Sleep(time.Second) // a send that ignores its ctx, like SMTP
		return nil
	})
	if !errors.Is(err, ErrDeliveryTimeout) {
		t.Fatalf("err = %v, want ErrDeliveryTimeout", err)
	}
	if err := n.deliver(context.Background(), func(ctx context.Context) error { return errors.New("boom") }); errors.Is(err, ErrDeliveryTimeout) {
		t.Fatalf("an ordinary failure was reported as a timeout: %v", err)
	}
}

// An org's own run budget replaces the default for its crons.
func TestCronRunUsesOrgBudget(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveShellFunction(custID, "", "spin", `while true; do :; done`, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, "", "spinner", "every 5m", "spin", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCronRunTimeout(custID, 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	ce := &CronExecutor{Store: db, Coord: c} // no RunTimeout: the default would be 2s
	w, _ := db.GetCronWatch(custID, "spinner")
	start := time.Now()
	ce.executeWatch(context.Background(), *w, TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("run took %s; the org's 150ms budget was ignored", took)
	}
	if w, _ = db.GetCronWatch(custID, "spinner"); w.LastStatus != "timeout" {
		t.Fatalf("last_status = %q, want timeout", w.LastStatus)
	}
}
