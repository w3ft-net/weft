package binscript_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

type fixture struct {
	svc   binscript.Service
	owner shape.Added
	ns    uint
}

func newFixture(t *testing.T) fixture {
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
	var owner shape.Added
	ns, err := srcs.FindOrCreateNamespace(owner, "ops")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{binscript.Service{Bins: bins, Crons: crons, Sources: srcs}, owner, ns.ID}
}

func (f fixture) cron(t *testing.T, name, pipeline string, text bool) {
	t.Helper()
	spec := cron.WatchSpec{Schedule: "every 5m", Pipeline: pipeline, Enabled: true}
	spec.Compiled.ValueType = cron.ValueNumber
	if text {
		spec.Compiled.ValueType = cron.ValueText
	}
	if err := f.svc.Crons.SaveWatch(f.owner, f.ns, "", name, spec); err != nil {
		t.Fatal(err)
	}
}

func TestSave_RejectsWhatValidateRejects(t *testing.T) {
	f := newFixture(t)
	for lang, src := range map[string]string{bin.LangBash: `echo hello; done`, bin.LangLua: "print(arg[1]"} {
		if err := f.svc.Save(f.owner, "", "broken-"+lang, src, lang); err == nil {
			t.Errorf("%s: expected a syntax error", lang)
		}
		if _, err := f.svc.Bins.Get(f.owner, "broken-"+lang); !errors.Is(err, bin.ErrNotFound) {
			t.Errorf("%s: broken script was stored: %v", lang, err)
		}
	}
}

func TestSave_TextCronsRefuseNumericReads(t *testing.T) {
	f := newFixture(t)
	f.cron(t, "holders", "# type: text\necho i-0abc", true)
	f.cron(t, "errors", "echo 1", false)

	for lang, src := range map[string]string{
		bin.LangLua:  `print(weft.avg("ops/holders", "1h"))`,
		bin.LangBash: `weft::avg ops/holders 1h`,
	} {
		err := f.svc.Save(f.owner, "", "avg-"+lang, src, lang)
		if err == nil || !strings.Contains(err.Error(), `cron "ops/holders" stores text`) {
			t.Errorf("%s: err = %v", lang, err)
		}
	}
	inline := `lua 'print(weft.avg("ops/holders", "1h"))'`
	if err := f.svc.Save(f.owner, "", "inline", inline, bin.LangBash); err == nil || !strings.Contains(err.Error(), "stores text") {
		t.Errorf("inline lua: err = %v", err)
	}
	if err := f.svc.Save(f.owner, "", "who", `print(weft.latest("ops/holders"))`, bin.LangLua); err != nil {
		t.Errorf("a text read is fine: %v", err)
	}
	if err := f.svc.Save(f.owner, "", "error-avg", `print(weft.avg("ops/errors", "1h"))`, bin.LangLua); err != nil {
		t.Errorf("a numeric read of a number cron is fine: %v", err)
	}
	if err := f.svc.Save(f.owner, "", "ghost", `print(weft.avg("ops/nothing", "1h"))`, bin.LangLua); err != nil {
		t.Errorf("a read of a cron that doesn't exist yet is fine: %v", err)
	}
	if err := f.svc.Save(f.owner, "", "bare", `print(weft.avg("holders", "1h"))`, bin.LangLua); err == nil || !strings.Contains(err.Error(), "no namespace") {
		t.Errorf("a name with no namespace: err = %v", err)
	}
}

func names(ws []cron.Watch) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}

func TestDependents(t *testing.T) {
	f := newFixture(t)
	save := func(name, src string) {
		t.Helper()
		if err := f.svc.Save(f.owner, "", name, src, bin.LangBash); err != nil {
			t.Fatal(err)
		}
	}
	save("inner", `echo hi`)
	save("outer", `/bin/inner | wc -l`)
	f.cron(t, "direct", "/bin/inner", false)
	f.cron(t, "indirect", "/bin/outer", false)
	f.cron(t, "unrelated", "echo 1", false)
	f.cron(t, "lookalike", "/bin/inner2", false)

	got, err := f.svc.Dependents(f.owner, "inner")
	if err != nil {
		t.Fatal(err)
	}
	if want := "direct indirect"; strings.Join(names(got), " ") != want {
		t.Errorf("dependents of inner = %v, want %s", names(got), want)
	}

	// A script nothing has created yet is still named by the crons waiting for it.
	f.cron(t, "waiting", "/bin/later", false)
	got, err = f.svc.Dependents(f.owner, "later")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(got), " ") != "waiting" {
		t.Errorf("dependents of later = %v", names(got))
	}

	// What a script calls is read when asked: no record to refresh.
	save("outer", `/bin/other | wc -l`)
	got, _ = f.svc.Dependents(f.owner, "inner")
	if strings.Join(names(got), " ") != "direct" {
		t.Errorf("after outer stopped calling inner: %v", names(got))
	}
}

func TestDelete_RefusedWhileCronsRunIt(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Save(f.owner, "", "scan", `echo hi`, bin.LangBash); err != nil {
		t.Fatal(err)
	}
	f.cron(t, "nightly", "/bin/scan", false)

	var inUse *binscript.InUseError
	if err := f.svc.Delete(f.owner, "scan"); !errors.As(err, &inUse) || len(inUse.Crons) != 1 {
		t.Fatalf("Delete = %v, want *InUseError naming one cron", err)
	}
	if err := f.svc.Crons.DeleteWatch(f.owner, f.ns, "nightly"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(f.owner, "scan"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Bins.Get(f.owner, "scan"); !errors.Is(err, bin.ErrNotFound) {
		t.Errorf("scan still there: %v", err)
	}
}
