package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

func TestOverdueCrons(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	w := func(id uint, sched string, last *time.Time, created time.Duration) store.CronWatch {
		cw := store.CronWatch{OrgID: 1, Name: "w", Schedule: sched, Enabled: true, LastRunAt: last}
		cw.ID = id
		cw.CreatedAt = now.Add(-created)
		return cw
	}
	disabled := w(5, "every 5m", ago(time.Hour), time.Hour)
	disabled.Enabled = false

	got := overdueCrons([]store.CronWatch{
		w(1, "every 5m", ago(4*time.Minute), time.Hour),         // fine
		w(2, "every 5m", ago(10*time.Minute), time.Hour),        // exactly 2x: not yet overdue
		w(3, "every 5m", ago(11*time.Minute), time.Hour),        // overdue
		w(4, "every 5m", nil, 30*time.Minute),                   // never ran, created long ago: overdue
		w(6, "every 5m", nil, time.Minute),                      // never ran, just created: fine
		disabled,                                                // disabled: never
		w(7, "not a schedule", ago(24*time.Hour), 48*time.Hour), // unrecognized runs at the 5m default, so overdue
		w(8, "every 0s", ago(24*time.Hour), 48*time.Hour),       // no usable interval: skipped
		w(9, "every 1m", ago(3*time.Minute), time.Hour),         // overdue at 1m cadence
	}, now)

	var ids []uint
	for _, o := range got {
		ids = append(ids, o.watch.ID)
	}
	want := []uint{3, 4, 7, 9}
	if len(ids) != len(want) {
		t.Fatalf("overdue ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("overdue ids = %v, want %v", ids, want)
		}
	}
}

type overduePage struct {
	org   store.OrgID
	alias string
	body  string
}

type overdueEnv struct {
	db      *store.Store
	org     store.OrgID
	mu      sync.Mutex
	sent    []overduePage
	sendErr error // returned from every send, to model an org with no alias
}

func newOverdueEnv(t *testing.T) *overdueEnv {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	adb := db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	org, err := db.FindOrCreateOrg("cron-overdue-test")
	if err != nil {
		t.Fatal(err)
	}
	e := &overdueEnv{db: db, org: org.ID}
	t.Cleanup(func() {
		adb.Exec(`DELETE FROM cron_watches WHERE org_id = ?`, org.ID)
		adb.Exec(`DELETE FROM leases WHERE name = ?`, cronOverdueLease)
		// Other coordinator tests assume the pooled fixture DB holds a
		// single org, so don't leave ours behind.
		adb.Exec(`DELETE FROM organizations WHERE id = ?`, org.ID)
		db.Close()
	})
	return e
}

func (e *overdueEnv) watch(t *testing.T, name, schedule string, lastRun *time.Time) store.CronWatch {
	t.Helper()
	w := store.CronWatch{OrgID: e.org, Name: name, Schedule: schedule, Pipeline: "true", Enabled: true, LastRunAt: lastRun}
	adb := e.db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	if err := adb.Create(&w).Error; err != nil {
		t.Fatal(err)
	}
	return w
}

func (e *overdueEnv) setLastRun(t *testing.T, id uint, at time.Time) {
	t.Helper()
	adb := e.db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	if err := adb.Model(&store.CronWatch{}).Where("id = ?", id).Update("last_run_at", at).Error; err != nil {
		t.Fatal(err)
	}
}

func (e *overdueEnv) executor(holder string) *CronExecutor {
	return &CronExecutor{
		Store:  e.db,
		Holder: holder,
		SendOverdue: func(_ context.Context, org store.OrgID, alias, _, body string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.sent = append(e.sent, overduePage{org, alias, body})
			return e.sendErr
		},
	}
}

// pagesFor returns the pages sent to this test's org. The pooled test DB may
// hold other orgs' watches, so everything is keyed by our own org.
func (e *overdueEnv) pagesFor() []overduePage {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []overduePage
	for _, p := range e.sent {
		if p.org == e.org {
			out = append(out, p)
		}
	}
	return out
}

func pagesMentioning(pages []overduePage, cron string) int {
	n := 0
	for _, p := range pages {
		if strings.Contains(p.body, cron+":") {
			n++
		}
	}
	return n
}

// The case that motivated using last_run_at: a cron that runs fine but has
// no output writes no cron_results rows. With a fresh last_run_at it must
// not be reported.
func TestOverdueQuietCronWithFreshRunIsNotReported(t *testing.T) {
	e := newOverdueEnv(t)
	now := time.Now()
	recent := now.Add(-time.Minute)
	w := e.watch(t, "od-quiet", "every 5m", &recent) // no cron_results rows exist for it
	// Created long ago, so only last_run_at can show it is healthy.
	adb := e.db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	if err := adb.Model(&store.CronWatch{}).Where("id = ?", w.ID).Update("created_at", now.Add(-48*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	e.executor("a").overdueTick(context.Background(), &overdueGuard{active: map[uint]bool{}}, now)
	if n := len(e.pagesFor()); n != 0 {
		t.Fatalf("a quiet cron that ran a minute ago caused %d page(s)", n)
	}
}

// Several crons stopping at once is ONE page for the org, to its reserved
// system alias, naming each of them. Nothing is decided per cron: a
// collector with no alert stage is reported like any other cron.
func TestOverdueTickSendsOneGroupedPagePerOrgToTheSystemAlias(t *testing.T) {
	e := newOverdueEnv(t)
	stale := time.Now().Add(-time.Hour)
	e.watch(t, "od-a", "every 5m", &stale)
	e.watch(t, "od-b", "every 5m", &stale)
	e.watch(t, "od-c", "every 10m", &stale)
	ce := e.executor("a")
	ce.overdueTick(context.Background(), &overdueGuard{active: map[uint]bool{}}, time.Now())

	pages := e.pagesFor()
	if len(pages) != 1 {
		t.Fatalf("got %d pages for 3 stopped crons in one org, want exactly 1", len(pages))
	}
	if pages[0].alias != store.SystemAlias {
		t.Errorf("paged alias %q, want the reserved %q", pages[0].alias, store.SystemAlias)
	}
	for _, name := range []string{"od-a", "od-b", "od-c"} {
		if pagesMentioning(pages, name) != 1 {
			t.Errorf("page does not name %s:\n%s", name, pages[0].body)
		}
	}
}

// A cron is reported once per episode. A cron that goes overdue later gets its
// own page naming only it; one that recovers and stops again is a new episode.
func TestOverdueTickReportsEachEpisodeOnce(t *testing.T) {
	e := newOverdueEnv(t)
	now := time.Now()
	stale := now.Add(-time.Hour)
	first := e.watch(t, "od-first", "every 5m", &stale)
	ce := e.executor("a")
	g := &overdueGuard{active: map[uint]bool{}}

	ce.overdueTick(context.Background(), g, now)
	ce.overdueTick(context.Background(), g, now.Add(time.Minute))
	if n := pagesMentioning(e.pagesFor(), "od-first"); n != 1 {
		t.Fatalf("a stopped cron was reported %d times across 2 ticks, want 1", n)
	}

	// A second cron stops later: a new page naming only it.
	second := e.watch(t, "od-second", "every 5m", &stale)
	ce.overdueTick(context.Background(), g, now.Add(2*time.Minute))
	pages := e.pagesFor()
	if len(pages) != 2 || strings.Contains(pages[1].body, "od-first:") || pagesMentioning(pages[1:], "od-second") != 1 {
		t.Fatalf("second page should name only od-second, pages: %+v", pages)
	}
	_ = second

	// The first recovers, then stops again: reported again.
	e.setLastRun(t, first.ID, now.Add(3*time.Minute))
	ce.overdueTick(context.Background(), g, now.Add(3*time.Minute))
	if g.active[first.ID] {
		t.Fatal("recovered cron still marked overdue")
	}
	ce.overdueTick(context.Background(), g, now.Add(time.Hour))
	if n := pagesMentioning(e.pagesFor(), "od-first"); n != 2 {
		t.Fatalf("second episode of od-first: reported %d times in total, want 2", n)
	}
}

// An org that has not defined the system alias is not paged: the send fails,
// that is only logged, and the cron is still tracked so it is not retried
// every minute.
func TestOverdueTickOrgWithoutSystemAliasIsNotRetried(t *testing.T) {
	e := newOverdueEnv(t)
	e.sendErr = errors.New(`alias "system" not defined in /etc/notify.yaml`)
	stale := time.Now().Add(-time.Hour)
	w := e.watch(t, "od-noalias", "every 5m", &stale)
	ce := e.executor("a")
	g := &overdueGuard{active: map[uint]bool{}}
	ce.overdueTick(context.Background(), g, time.Now())
	ce.overdueTick(context.Background(), g, time.Now().Add(time.Minute))
	if !g.active[w.ID] {
		t.Fatal("overdue cron not tracked")
	}
	if n := len(e.pagesFor()); n != 1 {
		t.Fatalf("delivery was attempted %d times for one episode, want 1", n)
	}
}

// A cron below its tier's minimum interval never runs, so it would be
// overdue forever; the guard must skip it.
func TestOverdueTickSkipsCronsThatCannotRun(t *testing.T) {
	e := newOverdueEnv(t)
	stale := time.Now().Add(-time.Hour)
	e.watch(t, "od-toofast", "every 1m", &stale) // trial minimum is 5m
	e.executor("a").overdueTick(context.Background(), &overdueGuard{active: map[uint]bool{}}, time.Now())
	if n := len(e.pagesFor()); n != 0 {
		t.Fatalf("a cron that cannot run caused %d page(s)", n)
	}
}

// Only the coordinator holding the guard lease checks; the other does
// nothing, so a stopped cron pages once and not once per instance.
func TestOverdueStepOnlyLeaseHolderChecks(t *testing.T) {
	e := newOverdueEnv(t)
	a, b := e.executor("coord-a"), e.executor("coord-b")
	ga := &overdueGuard{active: map[uint]bool{}}
	gb := &overdueGuard{active: map[uint]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !a.overdueStep(ctx, ga, time.Now()) {
		t.Fatal("first coordinator should have taken the guard lease")
	}
	if b.overdueStep(ctx, gb, time.Now()) {
		t.Fatal("second coordinator checked while the first holds the lease")
	}
	if !a.overdueStep(ctx, ga, time.Now()) {
		t.Fatal("holder should keep checking")
	}
}

// End to end through the real Notifier and the real notify.yaml parser: an
// org that defines the reserved `system` alias, here as a webhook, receives
// the grouped page. The alias is an ordinary one, so no parser change.
func TestOverduePageIsDeliveredThroughTheSystemAlias(t *testing.T) {
	e := newOverdueEnv(t)
	var got webhookPayload
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := e.db.SaveNotifyAliases(e.org, "", "system:\n  webhook:\n    - "+srv.URL+"\n"); err != nil {
		t.Fatal(err)
	}

	stale := time.Now().Add(-time.Hour)
	e.watch(t, "od-e2e-a", "every 5m", &stale)
	e.watch(t, "od-e2e-b", "every 5m", &stale)
	ce := &CronExecutor{Store: e.db, Holder: "a", Notifier: NewNotifier(e.db, nil)}
	ce.overdueTick(context.Background(), &overdueGuard{active: map[uint]bool{}}, time.Now())

	if hits != 1 {
		t.Fatalf("webhook received %d requests, want 1 grouped page", hits)
	}
	if got.Alias != store.SystemAlias || !strings.Contains(got.Body, "od-e2e-a:") || !strings.Contains(got.Body, "od-e2e-b:") {
		t.Fatalf("unexpected page: %+v", got)
	}
}
