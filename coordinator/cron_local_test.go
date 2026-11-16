package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// The cron executor runs over the cron contract and the coordination contract:
// here the open build's SQLite stores, with no Postgres behind either.
func TestCronExecutor_OverTheLocalStores(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := defaults.NewSourceBackend(db); err != nil { // the namespaces a cron is homed in
		t.Fatal(err)
	}
	crons, err := defaults.NewCronBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := defaults.NewCoordinationBackend(db)
	if err != nil {
		t.Fatal(err)
	}

	owner := shape.Added{}
	spec := func(enabled bool) cron.WatchSpec {
		return cron.WatchSpec{Schedule: "every 5m", Pipeline: "true", Enabled: enabled, Compiled: cron.Compiled{ValueType: cron.ValueNumber}}
	}
	if err := crons.SaveWatch(owner, 0, "", "on", spec(true)); err != nil {
		t.Fatal(err)
	}
	if err := crons.SaveWatch(owner, 0, "", "off", spec(false)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var fired []string
	ce := &CronExecutor{
		Crons: crons, Claims: claims, Holder: "me",
		Execute: func(_ context.Context, w cron.Watch, _ TimeRange) {
			mu.Lock()
			fired = append(fired, w.Name)
			mu.Unlock()
		},
	}
	ce.cancels = map[uint]context.CancelFunc{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Reconcile starts a loop for the enabled cron only.
	ce.reconcile(ctx)
	on, err := crons.GetWatch(owner, 0, "on")
	if err != nil {
		t.Fatal(err)
	}
	off, _ := crons.GetWatch(owner, 0, "off")
	ce.mu.Lock()
	_, onRuns := ce.cancels[on.ID]
	_, offRuns := ce.cancels[off.ID]
	ce.mu.Unlock()
	if !onRuns || offRuns {
		t.Fatalf("reconcile started on=%v off=%v, want only the enabled cron", onRuns, offRuns)
	}
	cancel() // the loops are not what this test drives
	ce.stopAll()

	// A due slot is claimed and fired once, however often the tick runs.
	st := &cronClaimState{watch: *on}
	now := on.CreatedAt.Add(20 * time.Minute)
	ce.claimTick(context.Background(), st, now)
	ce.claimTick(context.Background(), st, now)
	mu.Lock()
	if len(fired) != 1 || fired[0] != "on" {
		t.Fatalf("fired %v, want one run of on", fired)
	}
	mu.Unlock()
	// A second coordinator sharing the claims does not fire the slot again.
	other := &CronExecutor{Crons: crons, Claims: claims, Holder: "other", Execute: ce.Execute}
	other.claimTick(context.Background(), &cronClaimState{watch: *on}, now)
	mu.Lock()
	if len(fired) != 1 {
		t.Fatalf("a second holder fired the claimed slot: %v", fired)
	}
	mu.Unlock()

	// What a run records goes through the contract.
	if err := ce.persistResults(*on, now, []cronPoint{{line: "7", value: 7}}, "", 0, 12*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	rs, err := crons.ListResults(on.ID, 10, "")
	if err != nil || len(rs) != 1 || rs[0].Value == nil || *rs[0].Value != 7 || rs[0].DurationMs != 12 {
		t.Fatalf("results = %+v, err=%v", rs, err)
	}
	// The strike counter benches a cron after the limit, with no store behind it.
	for i := 1; i < BenchStrikes; i++ {
		if benched, err := ce.crons().RecordStrike(context.Background(), on.ID, BenchStrikes); err != nil || benched {
			t.Fatalf("strike %d: benched=%v err=%v", i, benched, err)
		}
	}
	if benched, err := ce.crons().RecordStrike(context.Background(), on.ID, BenchStrikes); err != nil || !benched {
		t.Fatalf("the last strike: benched=%v err=%v", benched, err)
	}
	if listed, _ := crons.ListEnabled(context.Background()); len(listed) != 0 {
		t.Errorf("a benched cron is still scheduled: %+v", listed)
	}
}
