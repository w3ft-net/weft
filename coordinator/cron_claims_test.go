package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// fakeClaimer is an in-memory coordination.Claims shared by several simulated
// coordinators. err, when set, fails every claim.
type fakeClaimer struct {
	mu     sync.Mutex
	owners map[string]string
	err    error
}

func (f *fakeClaimer) ClaimCronSlot(key, holder string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if f.owners == nil {
		f.owners = map[string]string{}
	}
	if _, taken := f.owners[key]; taken {
		return false, nil
	}
	f.owners[key] = holder
	return true, nil
}

// DeleteCronClaimsBefore removes nothing: the fake keeps no claim times.
func (f *fakeClaimer) DeleteCronClaimsBefore(time.Time) (int64, error) { return 0, nil }

type firedRun struct {
	holder string
	tr     TimeRange
}

type claimEnv struct {
	db      *sql.DB
	crons   *defaults.CronBackend
	watch   cron.Watch
	claims  *fakeClaimer
	mu      sync.Mutex
	fired   []firedRun
	cleanup func()
}

// newClaimEnv creates a committed cron watch (the claim tick reloads it
// from the database) with a fake claim backend.
func newClaimEnv(t *testing.T, name string) *claimEnv {
	t.Helper()
	return newClaimEnvIn(t, name, "")
}

// setCreatedAt backdates (or dates) the cron: the store stamps it at save.
func (e *claimEnv) setCreatedAt(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := e.db.Exec(`UPDATE cron_watches SET created_at = ? WHERE id = ?`, at.UTC().Format("2006-01-02T15:04:05.000000000Z"), e.watch.ID); err != nil {
		t.Fatal(err)
	}
	e.watch.CreatedAt = at
}

// newClaimEnvIn is newClaimEnv with the cron homed in the named namespace
// ("" is the owner level).
func newClaimEnvIn(t *testing.T, name, namespace string) *claimEnv {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sources, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	crons, err := defaults.NewCronBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	var home uint
	if namespace != "" {
		ns, err := sources.FindOrCreateNamespace(shape.Added{}, namespace)
		if err != nil {
			t.Fatal(err)
		}
		home = ns.ID
	}
	spec := cron.WatchSpec{Schedule: "every 5m", Pipeline: "true", Enabled: true, Compiled: cron.Compiled{ValueType: cron.ValueNumber}}
	if err := crons.SaveWatch(shape.Added{}, home, "", name, spec); err != nil {
		t.Fatal(err)
	}
	w, err := crons.GetWatch(shape.Added{}, home, name)
	if err != nil {
		t.Fatal(err)
	}
	e := &claimEnv{db: db, crons: crons, watch: *w, claims: &fakeClaimer{}}
	// An existing cron: created long before any slot a test simulates, so the
	// creation rule (see claimTick) does not skip its slots.
	e.setCreatedAt(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	return e
}

func (e *claimEnv) executor(holder string) *CronExecutor {
	return &CronExecutor{
		Holder: holder, Claims: e.claims, Crons: e.crons,
		Execute: func(_ context.Context, _ cron.Watch, tr TimeRange) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.fired = append(e.fired, firedRun{holder, tr})
		},
	}
}

func (e *claimEnv) runs() []firedRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]firedRun(nil), e.fired...)
	sort.Slice(out, func(i, j int) bool { return out[i].tr.Start.Before(out[j].tr.Start) })
	return out
}

// Several coordinators ticking against one claim backend, through a
// coordinator restart: every slot runs exactly once, and consecutive
// windows tile with no gap or overlap.
func TestClaimTicksRunEachSlotOnceAcrossCoordinators(t *testing.T) {
	env := newClaimEnv(t, "claims-sim")
	interval := 5 * time.Minute
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	type coord struct {
		ce *CronExecutor
		st *cronClaimState
	}
	mk := func(h string) *coord {
		return &coord{env.executor(h), &cronClaimState{watch: env.watch}}
	}
	coords := []*coord{mk("a"), mk("b"), mk("c")}

	const sim = 2 * time.Hour
	for at := time.Duration(0); at < sim; at += time.Second {
		now := t0.Add(at)
		// Halfway through, coordinator a is replaced by a fresh process
		// that has no memory of which slots were decided.
		if at == sim/2 {
			coords[0] = mk("a2")
		}
		for _, c := range coords {
			c.ce.claimTick(context.Background(), c.st, now)
		}
	}

	runs := env.runs()
	// Slots whose fire time falls in [t0, t0+2h): boundaries every 5m,
	// shifted by settle+jitter; the first partial slot may or may not
	// have fired before t0, so check the interior by tiling.
	if len(runs) < int(sim/interval)-1 {
		t.Fatalf("only %d runs in %s", len(runs), sim)
	}
	// A run's window is exactly one interval (never reaching older data than
	// that: the cold-shard fan-out incident).
	for i, r := range runs {
		if span := r.tr.End.Sub(r.tr.Start); span != interval-time.Microsecond {
			t.Fatalf("run %d window spans %v, want one interval less 1us", i, span)
		}
	}
	for i := 1; i < len(runs); i++ {
		prev, cur := runs[i-1].tr, runs[i].tr
		if want := prev.End.Add(time.Microsecond); !cur.Start.Equal(want) {
			t.Fatalf("run %d window starts %v, want %v (duplicate or gap after %v)", i, cur.Start, want, prev.End)
		}
	}
}

// A cron homed in a namespace fires like any other: its tick reloads the same
// row, not an org-level cron of the same name.
func TestClaimTickFiresNamespacedCron(t *testing.T) {
	env := newClaimEnvIn(t, "errors", "alpha")
	ce := env.executor("a")
	st := &cronClaimState{watch: env.watch}

	for at := time.Duration(0); at < 10*time.Minute; at += time.Second {
		ce.claimTick(context.Background(), st, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC).Add(at))
	}
	if len(env.runs()) == 0 {
		t.Fatal("a namespaced cron never fired")
	}
}

// A failed claim write must never run the slot, and the slot is retried.
func TestClaimTickFailsClosedAndRetries(t *testing.T) {
	env := newClaimEnv(t, "claims-failclosed")
	ce := env.executor("a")
	st := &cronClaimState{watch: env.watch}
	// Past slot 12:05's fire time (boundary + settle + jitter < 60s).
	now := time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC)

	env.claims.err = errors.New("db down")
	for i := 0; i < 3; i++ {
		ce.claimTick(context.Background(), st, now.Add(time.Duration(i)*time.Second))
	}
	if n := len(env.runs()); n != 0 {
		t.Fatalf("ran %d times with the claim store failing", n)
	}

	env.claims.err = nil
	ce.claimTick(context.Background(), st, now.Add(4*time.Second))
	ce.claimTick(context.Background(), st, now.Add(5*time.Second))
	if n := len(env.runs()); n != 1 {
		t.Fatalf("want exactly 1 run after the store recovered, got %d", n)
	}
}

// A cron created after a slot's window closed has nothing to report for it:
// that slot is skipped (not claimed, not run), and its first run is the next
// slot. Otherwise a new cron would evaluate, and could alert on, data from
// before it existed.
func TestClaimTickSkipsSlotsThatClosedBeforeTheCronWasCreated(t *testing.T) {
	env := newClaimEnv(t, "claims-created")
	env.setCreatedAt(t, time.Date(2026, 10, 4, 12, 7, 0, 0, time.UTC))
	ce := env.executor("a")
	st := &cronClaimState{watch: env.watch}

	// 12:06: the latest due slot closed at 12:05, before the cron existed.
	ce.claimTick(context.Background(), st, time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC))
	if n := len(env.runs()); n != 0 || len(env.claims.owners) != 0 {
		t.Fatalf("pre-creation slot ran %d time(s), claimed %v", n, env.claims.owners)
	}

	// 12:11: the 12:10 slot closed after creation, so it runs once.
	ce.claimTick(context.Background(), st, time.Date(2026, 10, 4, 12, 11, 5, 0, time.UTC))
	ce.claimTick(context.Background(), st, time.Date(2026, 10, 4, 12, 11, 6, 0, time.UTC))
	if n := len(env.runs()); n != 1 {
		t.Fatalf("want exactly 1 run for the first post-creation slot, got %d", n)
	}
}

// A won slot mints a fresh trace ID, and the firing receives a ctx
// carrying it (cron firings have no inbound request to inherit one from).
func TestClaimTickMintsTraceForFiring(t *testing.T) {
	env := newClaimEnv(t, "claims-trace")
	var got []string
	ce := env.executor("a")
	ce.Execute = func(ctx context.Context, _ cron.Watch, _ TimeRange) {
		id, _ := weftsdk.TraceIDFromContext(ctx)
		got = append(got, id)
	}
	st := &cronClaimState{watch: env.watch}
	ce.claimTick(context.Background(), st, time.Date(2026, 10, 4, 12, 6, 0, 0, time.UTC))
	ce.claimTick(context.Background(), st, time.Date(2026, 10, 4, 12, 11, 6, 0, time.UTC))
	if len(got) != 2 || got[0] == "" || got[1] == "" || got[0] == got[1] {
		t.Fatalf("firings saw trace IDs %q, want two distinct non-empty IDs", got)
	}
}
