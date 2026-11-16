package coordtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/cron/cronsched"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/warning"
)

// CronRuns runs the suites of a cron firing, as the executor does on a tick:
// the pipeline it runs, what the run records, and when it is benched. Each test
// gets a fresh Subject.
func CronRuns(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Subject)
	}{
		{"ShareScriptAcrossWatches", cronShareScriptAcrossWatches},
		{"BareNameStillWorks", cronBareNameStillWorks},
		{"BenchedAfterRepeatedTimeouts", cronBenchedAfterRepeatedTimeouts},
		{"BenchedAfterRepeatedDeliveryTimeouts", cronBenchedAfterRepeatedDeliveryTimeouts},
		{"OutputThatBreaksTheContractDisables", cronOutputThatBreaksTheContractDisables},
		{"OutputIsStoredAsPointsByTag", cronOutputIsStoredAsPointsByTag},
		{"InlineLuaReadsCronHistory", cronInlineLuaReadsCronHistory},
		{"TextCronStoresTextAndRefusesNumericReads", cronTextStoresTextAndRefusesNumericReads},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newSubject(t)) })
	}
}

// saveCron saves an owner-level cron the way the save path does, without
// lowering it: the fire path then plans inline.
func (s Subject) saveCron(t *testing.T, name, schedule, pipeline string) {
	t.Helper()
	s.saveCronIn(t, "", name, schedule, pipeline)
}

// saveCronIn is saveCron homed in namespace ns, "" for the owner level.
func (s Subject) saveCronIn(t *testing.T, ns, name, schedule, pipeline string) {
	t.Helper()
	if _, err := cronsched.ValidateSchedule(schedule); err != nil {
		t.Fatal(err)
	}
	compiled, err := cronauthor.Compile(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	spec := cron.WatchSpec{Schedule: schedule, Pipeline: pipeline, Enabled: true, Compiled: compiled.Compiled}
	if err := s.Crons.SaveWatch(s.Owner, s.home(t, ns), "", name, spec); err != nil {
		t.Fatal(err)
	}
}

// home is the id of namespace ns, made if need be; 0 for "", the owner level.
func (s Subject) home(t *testing.T, ns string) uint {
	t.Helper()
	if ns == "" {
		return 0
	}
	n, err := s.Sources.FindOrCreateNamespace(s.Owner, ns)
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

// watch is the saved owner-level cron called name.
func (s Subject) watch(t *testing.T, name string) cron.Watch { return s.watchIn(t, "", name) }

// watchIn is the saved cron called name homed in namespace ns.
func (s Subject) watchIn(t *testing.T, ns, name string) cron.Watch {
	t.Helper()
	w, err := s.Crons.GetWatch(s.Owner, s.home(t, ns), name)
	if err != nil {
		t.Fatal(err)
	}
	return *w
}

func (s Subject) saveBin(t *testing.T, name, source string) {
	t.Helper()
	if err := s.Bins.Save(s.Owner, "", name, source, "bash"); err != nil {
		t.Fatalf("save script %s: %v", name, err)
	}
}

// fire runs the cron once under ce and returns it as saved afterwards.
func (s Subject) fire(t *testing.T, ce *coordinator.CronExecutor, name string) cron.Watch {
	t.Helper()
	return s.fireIn(t, ce, "", name)
}

// fireIn is fire for the cron homed in namespace ns.
func (s Subject) fireIn(t *testing.T, ce *coordinator.CronExecutor, ns, name string) cron.Watch {
	t.Helper()
	tr := coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	ce.ExecuteWatch(context.Background(), s.watchIn(t, ns, name), tr)
	return s.watchIn(t, ns, name)
}

// executor is a cron executor over c with a recording system pager.
func (s Subject) executor(c *coordinator.Coordinator, n *coordinator.Notifier, timeout time.Duration, pages *[]string) *coordinator.CronExecutor {
	return &coordinator.CronExecutor{
		Claims: s.Claims, Coord: c, Notifier: n, RunTimeout: timeout,
		SendSystem: func(_ context.Context, owner shape.Added, alias, _, body string) error {
			if owner != s.Owner || alias != notifycfg.SystemAlias {
				return fmt.Errorf("page went to %+v alias %q, want %+v alias %q", owner, alias, s.Owner, notifycfg.SystemAlias)
			}
			*pages = append(*pages, body)
			return nil
		},
	}
}

// One /bin script, two crons passing different args. Each run dispatches the
// script's body with the cron's arg bound, producing distinct output per cron.
// This is the path a real cron tick takes.
func cronShareScriptAcrossWatches(t *testing.T, s Subject) {
	s.saveBin(t, "announce", `echo "source=$1"`)
	s.saveCron(t, "disk-check", "every 1m", "announce disk.log")
	s.saveCron(t, "cpu-check", "every 1m", "announce cpu.log")
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	tr := coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	for name, want := range map[string]string{"disk-check": "source=disk.log", "cpu-check": "source=cpu.log"} {
		w := s.watch(t, name)
		res, err := c.ExecuteForWatch(context.Background(), strings.TrimSpace(w.Pipeline), tr, &w)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := res.Drain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(d.Text); got != want {
			t.Errorf("%s: got %q, want %s (stderr=%q)", name, got, want, d.Stderr)
		}
	}
}

// A bare script name as the whole pipeline resolves through the coordinator's
// pre-expansion path.
func cronBareNameStillWorks(t *testing.T, s Subject) {
	s.saveBin(t, "hello", `echo hello`)
	s.saveCron(t, "greeter", "every 1m", "hello")
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	w := s.watch(t, "greeter")
	tr := coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	res, err := c.ExecuteForWatch(context.Background(), strings.TrimSpace(w.Pipeline), tr, &w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(d.Text); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

// A cron that keeps blowing its run budget is disabled; one run inside the
// budget resets the count.
func cronBenchedAfterRepeatedTimeouts(t *testing.T, s Subject) {
	s.saveBin(t, "slowpoke", `while true; do :; done`)
	s.saveBin(t, "quick", `echo 1`)
	s.saveCron(t, "slow", "every 5m", "slowpoke")
	s.saveCron(t, "fast", "every 5m", "quick")
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	var pages []string
	ce := s.executor(c, nil, 100*time.Millisecond, &pages)

	for i := 1; i <= coordinator.BenchStrikes; i++ {
		w := s.fire(t, ce, "slow")
		if w.LastStatus != cron.StatusTimeout {
			t.Fatalf("run %d: last status = %q, want %s", i, w.LastStatus, cron.StatusTimeout)
		}
		if w.Enabled != (i < coordinator.BenchStrikes) {
			t.Fatalf("run %d: enabled = %v", i, w.Enabled)
		}
		if want := i / coordinator.BenchStrikes; len(pages) != want {
			t.Fatalf("after run %d: %d page(s), want %d (only the benching run pages)", i, len(pages), want)
		}
	}
	if !strings.Contains(pages[0], "slow") || !strings.Contains(pages[0], "# enabled: false") {
		t.Errorf("page should name the cron and say how to re-enable it:\n%s", pages[0])
	}

	// A budget the fast run is sure to meet: 100ms is only there to make the
	// slow one time out, and a loaded CI runner can spend that starting bash.
	ce.RunTimeout = 10 * time.Second
	if w := s.fire(t, ce, "fast"); w.LastStatus != cron.StatusOK || !w.Enabled || w.ConsecutiveStrikes != 0 {
		t.Fatalf("fast cron: status=%q enabled=%v strikes=%d", w.LastStatus, w.Enabled, w.ConsecutiveStrikes)
	}
}

// The strike rule isn't about the pipeline alone: a cron whose notification
// target keeps timing out is benched too, and a tick whose deliveries go through
// clears the count.
func cronBenchedAfterRepeatedDeliveryTimeouts(t *testing.T, s Subject) {
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
		if err := s.Notify.Save(s.Owner, "", yaml); err != nil {
			t.Fatal(err)
		}
	}
	setHook(dead.URL)
	s.saveCron(t, "deaf-hook", "every 5m", "echo 1 | alert above 0 --notify=hook")

	n := coordinator.NewNotifier(s.Notify)
	n.Timeout = 100 * time.Millisecond
	c := s.coordinator(t, coordinator.CoordinatorConfig{Notifier: n})
	var pages []string
	ce := s.executor(c, n, 5*time.Second, &pages)

	s.fire(t, ce, "deaf-hook")
	if w := s.fire(t, ce, "deaf-hook"); w.ConsecutiveStrikes != 2 || !w.Enabled {
		t.Fatalf("after two dead ticks: strikes=%d enabled=%v, want 2/true", w.ConsecutiveStrikes, w.Enabled)
	}
	setHook(live.URL)
	if w := s.fire(t, ce, "deaf-hook"); w.ConsecutiveStrikes != 0 || !w.Enabled {
		t.Fatalf("after a delivered tick: strikes=%d enabled=%v, want 0/true", w.ConsecutiveStrikes, w.Enabled)
	}

	// The endpoint goes dead again; the incident is still open, and each tick
	// retries the target that missed it.
	setHook(dead.URL)
	var w cron.Watch
	for i := 0; i < coordinator.BenchStrikes; i++ {
		w = s.fire(t, ce, "deaf-hook")
	}
	if w.Enabled {
		t.Fatalf("still enabled after %d delivery timeouts in a row (strikes=%d)", coordinator.BenchStrikes, w.ConsecutiveStrikes)
	}
	if len(pages) != 1 {
		t.Errorf("%d page(s) for the benched cron, want 1", len(pages))
	}
}

// Output that breaks the contract stores no points and disables the cron on its
// first run with a page that names the check.
func cronOutputThatBreaksTheContractDisables(t *testing.T, s Subject) {
	s.saveBin(t, "wide", `echo "1 a extra"`)
	s.saveCron(t, "wide", "every 5m", "wide")
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	var pages []string
	ce := s.executor(c, nil, 5*time.Second, &pages)

	if w := s.fire(t, ce, "wide"); w.LastStatus != cron.StatusInvalid || w.Enabled || w.ConsecutiveStrikes != 0 {
		t.Fatalf("status=%q enabled=%v strikes=%d, want invalid, disabled, no strike", w.LastStatus, w.Enabled, w.ConsecutiveStrikes)
	}
	if len(pages) != 1 || !strings.Contains(pages[0], "line 1 has 3 fields") {
		t.Fatalf("pages = %q, want one naming the check", pages)
	}
	rows, err := s.Crons.ListResults(s.watch(t, "wide").ID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Value != nil || r.Stdout != "" || !strings.Contains(r.Stderr, "3 fields") {
			t.Fatalf("an invalid run stored %+v; want only the reason it was disabled, no value", r)
		}
	}
}

// A run's points are stored per tag, whether the cron prints "<value> <tag>"
// lines or declares JSON schema headers in its pipeline comments.
func cronOutputIsStoredAsPointsByTag(t *testing.T, s Subject) {
	s.saveBin(t, "hosts", `printf '3 api-1\n4 api-2\n'`)
	s.saveCron(t, "hosts", "every 5m", "hosts")
	s.saveCron(t, "json", "every 5m", "# value: .pct\n# tag: .host\necho '{\"host\":\"db-1\",\"pct\":12.5}'")
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	var pages []string
	ce := s.executor(c, nil, 5*time.Second, &pages)

	if w := s.fire(t, ce, "hosts"); w.LastStatus != cron.StatusOK || w.ConsecutiveStrikes != 0 {
		t.Fatalf("hosts: status=%q strikes=%d", w.LastStatus, w.ConsecutiveStrikes)
	}
	for tag, want := range map[string]float64{"api-1": 3, "api-2": 4} {
		rows, err := s.Crons.ListResults(s.watch(t, "hosts").ID, 1, tag)
		if err != nil || len(rows) != 1 || rows[0].Value == nil || *rows[0].Value != want {
			t.Fatalf("tag %s: rows=%+v err=%v, want value %v", tag, rows, err, want)
		}
	}

	if w := s.fire(t, ce, "json"); w.LastStatus != cron.StatusOK {
		t.Fatalf("json: status=%q", w.LastStatus)
	}
	if rows, err := s.Crons.ListResults(s.watch(t, "json").ID, 1, "db-1"); err != nil || len(rows) != 1 || *rows[0].Value != 12.5 {
		t.Fatalf("json rows=%+v err=%v, want db-1 = 12.5", rows, err)
	}
}

// A cron's bash can run Lua inline, and that Lua reads cron history through the
// weft module. Here an inline program averages another cron and the run stores
// the result; the same program aimed at a text cron is refused at save, and one
// that builds the name at run time is refused when it runs.
func cronInlineLuaReadsCronHistory(t *testing.T, s Subject) {
	errs := s.saveWatch(t, "ops", "errors")
	s.record(t, errs, "", 2, 4, 6)
	s.saveCronIn(t, "ops", "error-avg", "every 5m", `lua 'print(weft.avg("ops/errors", "1h"))' | alert above 5`)
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	var pages []string
	ce := s.executor(c, nil, 5*time.Second, &pages)

	w := s.fireIn(t, ce, "ops", "error-avg")
	if w.LastStatus != cron.StatusOK {
		t.Fatalf("status = %q", w.LastStatus)
	}
	rows, err := s.Crons.LatestRun(w.ID)
	if err != nil || len(rows) != 1 || rows[0].Value == nil || *rows[0].Value != 4 {
		t.Fatalf("latest = %+v err=%v, want avg 4", rows, err)
	}

	s.saveCronIn(t, "ops", "holders", "every 5m", "# type: text\necho i-0abc")
	// A save on a user's behalf goes through the cron author, which checks what
	// the pipeline reads against what the cron it reads stores.
	author := func(name, pipeline string) error {
		err := cronauthor.Service{Crons: s.Crons, Sources: s.Sources, Bins: s.Bins}.Save(context.Background(), cronauthor.Request{
			Owner: s.Owner, Home: s.home(t, "ops"), Name: name,
			Schedule: "every 5m", Pipeline: pipeline, Enabled: true,
		})
		blocking, _ := warning.Split(err)
		return errors.Join(blocking...)
	}
	if err := author("holder-avg", `lua 'print(weft.avg("ops/holders", "1h"))'`); err == nil || !strings.Contains(err.Error(), `cron "ops/holders" stores text`) {
		t.Fatalf("inline Lua reading a text cron saved: %v", err)
	}
	if err := author("holder-avg", `lua 'print(weft.avg("ops/hol" .. "ders", "1h"))'`); err != nil {
		t.Fatalf("a name built at run time can't be checked at save: %v", err)
	}
	// The run-time check fails it the way a save would have, so one run disables
	// it, with one page saying why, rather than three strikes.
	if w := s.fireIn(t, ce, "ops", "holder-avg"); w.LastStatus != cron.StatusError || w.Enabled || w.ConsecutiveStrikes != 0 {
		t.Fatalf("holder-avg status=%q enabled=%v strikes=%d, want error, disabled, no strike", w.LastStatus, w.Enabled, w.ConsecutiveStrikes)
	}
	if len(pages) != 1 || !strings.Contains(pages[0], `cron "ops/holders" stores text`) || !strings.Contains(pages[0], "# enabled: false") {
		t.Fatalf("pages = %q, want one naming the text cron and how to re-enable", pages)
	}
	if res, _ := s.Crons.ListResults(s.watchIn(t, "ops", "holder-avg").ID, 1, ""); len(res) != 1 || !strings.Contains(res[0].Stderr, "stores text") {
		t.Fatalf("holder-avg stored %+v, want the stores-text error", res)
	}
	// Bash gets there the same way.
	s.saveCronIn(t, "ops", "holder-avg-bash", "every 5m", `n=ops/holders; weft::avg "$n" 1h`)
	if w := s.fireIn(t, ce, "ops", "holder-avg-bash"); w.Enabled || len(pages) != 2 {
		t.Fatalf("bash dynamic name: enabled=%v pages=%d, want disabled with a second page", w.Enabled, len(pages))
	}

	// A clean cron is untouched by it.
	if w := s.fireIn(t, ce, "ops", "error-avg"); !w.Enabled || len(pages) != 2 {
		t.Fatalf("error-avg enabled=%v pages=%d after another cron's type error", w.Enabled, len(pages))
	}
}

// A text cron stores its values as text with no number, so the rollups never see
// them; numeric weft:: functions refuse it at run time when the save couldn't
// see the name.
func cronTextStoresTextAndRefusesNumericReads(t *testing.T, s Subject) {
	s.saveBin(t, "holders", `printf 'i-0abc lease-a\ni-0def lease-b\n'`)
	s.saveCronIn(t, "ops", "holders", "every 5m", "# type: text\nholders")
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	var pages []string
	ce := s.executor(c, nil, 5*time.Second, &pages)

	w := s.fireIn(t, ce, "ops", "holders")
	if w.LastStatus != cron.StatusOK {
		t.Fatalf("status = %q", w.LastStatus)
	}
	rows, err := s.Crons.LatestRun(w.ID)
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

	// The weft:: functions, as a user types them.
	sh := func(line string) (text, stderr string) {
		t.Helper()
		text, stderr, err := s.run(t, c, line)
		if err != nil {
			stderr += err.Error()
		}
		return text, stderr
	}
	if _, stderr := sh(`weft::avg ops/holders 1h`); !strings.Contains(stderr, `cron "ops/holders" stores text`) {
		t.Fatalf("weft::avg of a text cron: stderr %q", stderr)
	}
	if _, stderr := sh(`weft::percentile ops/holders 1h 0.5`); !strings.Contains(stderr, "stores text") {
		t.Fatalf("weft::percentile of a text cron: stderr %q", stderr)
	}
	if _, stderr := sh(`weft::is_absent ops/holders 1h`); strings.Contains(stderr, "stores text") {
		t.Fatalf("weft::is_absent reads text crons: stderr %q", stderr)
	}
	if text, stderr := sh(`weft::latest ops/holders`); strings.TrimSpace(text) == "" {
		t.Fatalf("weft::latest = %q, stderr %q", text, stderr)
	}
	if text, _ := sh(`weft::describe ops/holders 2`); strings.Count(text, "\n") != 2 || !strings.Contains(text, "i-0abc") {
		t.Fatalf("weft::describe = %q; want two runs with text values", text)
	}
	// A bash predicate reports the mistake (exit 2) instead of answering "can't
	// tell" (exit 1), which would read as the condition not holding.
	if text, stderr := sh(`weft::is_above ops/holders 5; echo "exit=$?"`); !strings.Contains(text, "exit=2") || !strings.Contains(stderr, "stores text") {
		t.Fatalf("weft::is_above on a text cron: text %q stderr %q", text, stderr)
	}
}
