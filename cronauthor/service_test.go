package cronauthor_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/sqlitedb"
	"github.com/w3ft-net/weft/warning"
)

type limits struct{ identity.Limits }

func (l limits) LimitsFor(shape.Added) identity.Limits  { return l.Limits }
func (limits) CronRunTimeout(shape.Added) time.Duration { return 0 }

type fixture struct {
	svc   cronauthor.Service
	owner shape.Added
	ns    uint
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bins, err := defaults.NewBinBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	crons, err := defaults.NewCronBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	owner := shapetest.Added(1)
	ns, err := srcs.FindOrCreateNamespace(owner, "ops")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{cronauthor.Service{Crons: crons, Sources: srcs, Bins: bins}, owner, ns.ID}
}

func (f *fixture) save(name, schedule, pipeline string, enabled bool) error {
	return f.svc.Save(context.Background(), cronauthor.Request{
		Owner: f.owner, Home: f.ns, Name: name, Schedule: schedule, Pipeline: pipeline, Enabled: enabled,
	})
}

func (f *fixture) mustSave(t *testing.T, name, pipeline string) {
	t.Helper()
	if err := f.save(name, "every 5m", pipeline, true); err != nil {
		t.Fatalf("save %s: %v", name, err)
	}
}

func TestSave_TextCronsAndTheirNumericReaders(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, "holders", "# type: text\necho i-0abc")
	f.mustSave(t, "errors", "echo 1")

	// A numeric reader of a text cron is refused; a text reader is fine.
	if err := f.save("avg-holders", "every 5m", "weft::avg ops/holders 1h", true); err == nil || !strings.Contains(err.Error(), `cron "ops/holders" stores text`) {
		t.Fatalf("numeric read of a text cron: %v", err)
	}
	f.mustSave(t, "latest-holders", "weft::latest ops/holders")

	// A cron can't read itself as numbers once it stores text.
	if err := f.save("selfish", "every 5m", "# type: text\nweft::avg ops/selfish 1h", true); err == nil || !strings.Contains(err.Error(), "can't read itself") {
		t.Fatalf("self read: %v", err)
	}

	// A cron can't turn text while numeric readers name it, and the refusal lists
	// them: other crons, /bin scripts, and whatever the deployment adds.
	f.mustSave(t, "errors-trend", "weft::slope ops/errors 6h | alert above 1")
	if err := f.svc.Bins.Save(f.owner, "", "report", `print(weft.avg("ops/errors", "1h"))`, bin.LangLua); err != nil {
		t.Fatal(err)
	}
	f.svc.Readers = func(_ shape.Added, path string) ([]string, error) {
		if path == "ops/errors" {
			return []string{"/etc/dashboards/ops.yaml (sparkline)"}, nil
		}
		return nil, nil
	}
	err := f.save("errors", "every 5m", "# type: text\necho busy", true)
	for _, want := range []string{"/bin/report", "/cron.d/ops/errors-trend", "/etc/dashboards/ops.yaml (sparkline)"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("turning a read cron text: %v, want it to name %s", err, want)
		}
	}
	// Nothing was written by the refusal.
	if w, _ := f.svc.Crons.GetWatch(f.owner, f.ns, "errors"); w.Compiled.ValueType != cron.ValueNumber {
		t.Fatalf("the refused save changed the cron: %+v", w.Compiled)
	}
	// Once text, saving it again is not a new turn.
	f.svc.Readers = nil
	if err := f.svc.Bins.Delete(f.owner, "report"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Crons.DeleteWatch(f.owner, f.ns, "errors-trend"); err != nil {
		t.Fatal(err)
	}
	f.mustSave(t, "errors", "# type: text\necho busy")
}

func TestSave_PlanLimits(t *testing.T) {
	f := newFixture(t)
	f.svc.Entitlements = limits{identity.Limits{MaxCrons: 2, MinCronInterval: 5 * time.Minute}}

	var limit *identity.PlanLimitError
	if err := f.save("fast", "every 1m", "echo 1", true); !errors.As(err, &limit) || !strings.Contains(err.Error(), "minimum schedule interval is 5m0s") {
		t.Fatalf("a schedule under the minimum: %v", err)
	}
	f.mustSave(t, "a", "echo 1")
	f.mustSave(t, "b", "echo 1")
	if err := f.save("c", "every 5m", "echo 1", true); !errors.As(err, &limit) || !strings.Contains(err.Error(), "cron limit reached (2 crons)") {
		t.Fatalf("a third enabled cron: %v", err)
	}
	// An edit of a cron that already holds a slot is not a new one.
	if err := f.save("a", "every 10m", "echo 2", true); err != nil {
		t.Fatalf("editing an enabled cron at the cap: %v", err)
	}
	// A disabled cron is free; turning it on takes a slot.
	if err := f.save("c", "every 5m", "echo 1", false); err != nil {
		t.Fatalf("a disabled cron at the cap: %v", err)
	}
	if err := f.save("c", "every 5m", "echo 1", true); !errors.As(err, &limit) {
		t.Fatalf("enabling a third cron: %v", err)
	}
	// Freeing a slot frees the cap.
	if err := f.svc.Crons.DeleteWatch(f.owner, f.ns, "b"); err != nil {
		t.Fatal(err)
	}
	f.mustSave(t, "c", "echo 1")
}

func TestSave_NoEntitlementsIsNoLimits(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := f.save(n, "every 1s", "echo 1", true); err != nil {
			t.Fatalf("save %s: %v", n, err)
		}
	}
}

// The caller's read check runs after the plan limits, before the pipeline is
// compiled, and a refusal writes nothing.
func TestSave_AuthorizeRunsBetweenLimitsAndCompile(t *testing.T) {
	f := newFixture(t)
	f.svc.Entitlements = limits{identity.Limits{MinCronInterval: 5 * time.Minute}}
	denied := errors.New("reads what you may not")
	req := cronauthor.Request{Owner: f.owner, Home: f.ns, Name: "x", Schedule: "every 5m", Pipeline: "# type: number\necho a", Enabled: true,
		Authorize: func(string) error { return denied }}

	// The pipeline doesn't compile (an unknown type), but authorization comes first.
	if err := f.svc.Save(context.Background(), req); !errors.Is(err, denied) {
		t.Fatalf("Save = %v, want the authorization refusal", err)
	}
	// The plan limit comes before authorization.
	req.Schedule = "every 1m"
	var limit *identity.PlanLimitError
	if err := f.svc.Save(context.Background(), req); !errors.As(err, &limit) {
		t.Fatalf("Save = %v, want the plan limit", err)
	}
	// Authorized, the compile error is the answer, and nothing is stored.
	req.Schedule, req.Authorize = "every 5m", func(string) error { return nil }
	if err := f.svc.Save(context.Background(), req); err == nil || !strings.Contains(err.Error(), "'# type:' takes text") {
		t.Fatalf("Save = %v, want the compile error", err)
	}
	if _, err := f.svc.Crons.GetWatch(f.owner, f.ns, "x"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("a refused save left a cron: %v", err)
	}
}

func TestSave_StampsTheSchemaAndLowersThePipeline(t *testing.T) {
	f := newFixture(t)
	f.svc.Entitlements = limits{identity.Limits{}}
	f.mustSave(t, "disk", "cat /log/x/y/main.log | awk '{print $3}' | alert above 80")

	w, err := f.svc.Crons.GetWatch(f.owner, f.ns, "disk")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Compiled.HasAlert || w.Compiled.AlertCondition == nil || *w.Compiled.AlertCondition != "above" || *w.Compiled.AlertThreshold != 80 {
		t.Errorf("compiled = %+v", w.Compiled)
	}
	if w.Compiled.ValueType != cron.ValueNumber {
		t.Errorf("value type = %q", w.Compiled.ValueType)
	}
	if len(w.LoweredPlan) == 0 {
		t.Error("the pipeline was not lowered at save")
	}
}

// A save that succeeds can still come back with something worth showing.
func TestSave_WarnsWithoutRejecting(t *testing.T) {
	f := newFixture(t)
	err := f.save("trunc", "every 5m", `tail -n 1000 /log/x/y/main.log | grep -c "boom"`, true)
	blocking, warnings := warning.Split(err)
	if len(blocking) != 0 {
		t.Fatalf("blocking = %v, want the save to succeed", blocking)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "caps this cron") {
		t.Fatalf("warnings = %q", warnings)
	}
	if _, err := f.svc.Crons.GetWatch(f.owner, f.ns, "trunc"); err != nil {
		t.Fatalf("the warned save was not stored: %v", err)
	}

	// A schedule that can reach cold data says so.
	err = f.save("long", "every 14d", "echo 1", true)
	if _, warnings := warning.Split(err); !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "cold history budget") }) {
		t.Fatalf("a 14-day cron: warnings = %q", warnings)
	}
}

func TestParseAndSerialize_RoundTrip(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		text := cronauthor.Serialize("*/10 * * * *", "echo 1 | alert any", enabled)
		schedule, pipeline, got, err := cronauthor.Parse(text)
		if err != nil || schedule != "*/10 * * * *" || pipeline != "echo 1 | alert any" || got != enabled {
			t.Errorf("enabled=%v: Parse(%q) = %q %q %v %v", enabled, text, schedule, pipeline, got, err)
		}
	}
	for _, bad := range []string{"echo 1", "# schedule: every 5m\n", "# schedule: every 5m\n# enabled: maybe\necho 1"} {
		if _, _, _, err := cronauthor.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

// A plain, uncapped pipeline saves clean: no warnings at all.
func TestSave_NoWarningForASafePipeline(t *testing.T) {
	f := newFixture(t)
	err := f.save("safe", "every 5m", `cat /log/x/y/main.log | grep ERROR`, true)
	blocking, warnings := warning.Split(err)
	if len(blocking) != 0 || len(warnings) != 0 {
		t.Fatalf("err = %v: blocking=%v warnings=%q, want a clean save", err, blocking, warnings)
	}
}

// The windows that can reach older data warn, in the customer's words, and
// shorter ones don't.
func TestSave_ColdBudgetWarningByWindow(t *testing.T) {
	f := newFixture(t)
	mentions := func(schedule string) ([]string, bool) {
		t.Helper()
		err := f.save("c-"+strings.NewReplacer(" ", "-", "*", "x").Replace(schedule), schedule, "cat vimgym.log | wc -l", true)
		blocking, warnings := warning.Split(err)
		if len(blocking) > 0 {
			t.Fatalf("%s: rejected: %v", schedule, blocking)
		}
		return warnings, slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "cold history budget") })
	}
	for _, schedule := range []string{"every 5m", "0 * * * *", "0 0 * * *", "every 7d", "every 12d"} {
		if ws, warned := mentions(schedule); warned {
			t.Errorf("%q warned about the cold budget: %v", schedule, ws)
		}
	}
	for _, schedule := range []string{"every 13d", "every 14d", "every 3w"} {
		ws, warned := mentions(schedule)
		if !warned {
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

// A cron pays for a stage that runs post-retrieval on every fire, so saving one
// explains it: a warning names the stage and what to change.
func TestSave_ExplainsAStageThatRunsPostRetrieval(t *testing.T) {
	f := newFixture(t)
	err := f.save("explain", "every 5m", `cat /log/x/y/main.log | awk '{ printf "%s\n", $0 }' | alert above 5`, true)
	blocking, warnings := warning.Split(err)
	if len(blocking) != 0 {
		t.Fatalf("blocking = %v, want success", blocking)
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	for _, want := range []string{"on every fire", "awk", "post-retrieval", "Barrier: stage 1"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning lacks %q:\n%s", want, warnings[0])
		}
	}
}

// A /bin script's own stages aren't analyzed: a plan that inlined one is never
// reported as post-retrieval, and a save whose lowering failed says nothing about
// stages at all.
func TestLint_NoExplanationForABinScript(t *testing.T) {
	pipeline := `cat /log/x/y/main.log | /bin/errs | alert above 5`
	inlined, err := json.Marshal(planner.LoweredPlan{BinDeps: []planner.BinDep{{Name: "errs"}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, plan := range map[string][]byte{"inlined": inlined, "not lowered": nil} {
		if blocking, warnings := warning.Split(cronauthor.Lint("every 5m", pipeline, plan)); len(blocking) != 0 || len(warnings) != 0 {
			t.Errorf("%s: blocking %v, warnings %q; want neither", name, blocking, warnings)
		}
	}
}

// A pipeline that doesn't parse is a rejection, never a warning.
func TestSave_ASyntaxErrorBlocks(t *testing.T) {
	f := newFixture(t)
	err := f.save("bad", "every 5m", `cat /log/x/y/main.log |`, true)
	if blocking, _ := warning.Split(err); len(blocking) == 0 {
		t.Fatal("want a blocking error for an unparseable pipeline, got none")
	}
}

// Schema headers stay in the stored pipeline, so a cron file round-trips through
// its own serialization with them in place.
func TestParse_KeepsSchemaHeaders(t *testing.T) {
	file := "# schedule: */5 * * * *\n# value: .pct\n# tag: .host\n# a note\ncat /log/a.log | jq -c .\n"
	schedule, pipeline, enabled, err := cronauthor.Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# value: .pct\n# tag: .host\ncat /log/a.log | jq -c ."; pipeline != want {
		t.Fatalf("pipeline = %q, want %q", pipeline, want)
	}
	got := cronauthor.Serialize(schedule, pipeline, enabled)
	if want := "# schedule: */5 * * * *\n# value: .pct\n# tag: .host\ncat /log/a.log | jq -c .\n"; got != want {
		t.Fatalf("serialized = %q, want %q", got, want)
	}
	if _, _, _, err := cronauthor.Parse("# schedule: every 5m\n# value: .pct\n"); err == nil {
		t.Fatal("a file with only headers saved without a pipeline")
	}
}
