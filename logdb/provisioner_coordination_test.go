// White-box coordination tests for the provisioner package. These drive
// the unexported tick (check) and the heartbeat path with the real bucket
// and shard SMs wired together — one level up from the per-transition
// unit tests in shardsm_test/bucketsm_test, but still white-box: they
// call unexported methods, set private fields (configurer), and use the
// single-connection tx-scoped test store with fake collaborators.
//
// This is NOT an integration test. It sits below the seam where
// production wires the connection pool and the launcher, which is why it
// can (and must) gate the launches around the single tx connection. The
// real wiring — a pooled store, a real gRPC Configure dial, genuinely
// concurrent launches — is covered in the integration/ package.
package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// inProcLauncher is an in-process launcher that satisfies BOTH
// ShardLauncher (NewProvisioner's constructor type) and ShardLaunchAPI
// (the FSM launch primitive shardSMDeps wires). Launch returns a unique
// synthetic instance id per call — the correlation key a simulated
// heartbeat carries back so ProcessHeartbeat can promote the right
// LAUNCHING shard. No real container; the test drives the heartbeat.
//
// The gate makes multi-shard launches deterministic. The FSM kicks one
// runLaunch goroutine per shard, each of which writes the store after
// Launch returns; the tx-scoped test store is a single connection that
// panics under concurrent use. When gate is set, Launch parks on it
// until the test releases one token at a time (drainLaunches), so the N
// goroutines' store writes serialize — without reducing Target to 1 and
// losing the "one bucket SM juggling N shard SMs" coverage.
type inProcLauncher struct {
	mu         sync.Mutex
	seq        int
	launched   []LaunchSpec
	terminated []string
	gate       chan struct{}
}

// Launch (ShardLaunchAPI) — the FSM path. The instance id is assigned at
// call time (before parking on the gate) so concurrent callers get
// distinct ids regardless of release order.
func (l *inProcLauncher) Launch(_ context.Context, spec LaunchSpec) (LaunchResult, error) {
	l.mu.Lock()
	l.seq++
	n := l.seq
	l.launched = append(l.launched, spec)
	gate := l.gate
	l.mu.Unlock()

	if gate != nil {
		<-gate // park until the test releases this launch
	}

	vol := spec.ExistingVolumeID
	if vol == "" {
		vol = fmt.Sprintf("vol-%d", n)
	}
	return LaunchResult{
		InstanceID: fmt.Sprintf("i-%d", n),
		VolumeID:   vol,
		AZ:         spec.AZ,
	}, nil
}

func (l *inProcLauncher) Terminate(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.terminated = append(l.terminated, id)
	return nil
}

// LaunchInstance / TerminateInstance (ShardLauncher) — unused by the FSM
// path, present only so NewProvisioner accepts this launcher.
func (l *inProcLauncher) LaunchInstance(context.Context, string) (string, error) {
	return "", fmt.Errorf("inProcLauncher: LaunchInstance is not the FSM path")
}
func (l *inProcLauncher) TerminateInstance(context.Context, string, string) error { return nil }

// newFSMTestProvisioner wires a real Provisioner over the test store with
// the in-process launcher and stub configurer, FSM control plane built.
func newFSMTestProvisioner(t *testing.T, db *store.Store) (*Provisioner, *inProcLauncher) {
	t.Helper()
	reg := NewShardRegistry()
	reg.SetStore(ShardStore{db})
	// Hydrate from the store the way Start() does, so a "restart"
	// provisioner over the same store sees persisted shards. reconstruct
	// fails for live (non-archived) rows — they load handle-less and
	// reconnect on the next heartbeat (ObserveHeartbeat).
	if err := reg.LoadFromStore(func(*Shard) (shard.Shard, error) {
		return nil, errNoReconstruct
	}); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	launcher := &inProcLauncher{gate: make(chan struct{})}
	// MaxConcurrentEvaluate: 1 — db is openSMTestStore's tx-scoped single
	// connection, which panics under concurrent use (see its doc comment);
	// evaluateStateMachines otherwise fans out bucket/shard Evaluate calls
	// concurrently in production.
	prov := NewProvisioner(reg, launcher, ProvisionerConfig{PollInterval: time.Second, MaxConcurrentEvaluate: 1})
	// Pin the clock to a fixed mid-window instant so window alignment
	// (CurrentWindow Truncate's to the hour for a 1h bucket) is
	// deterministic across a multi-tick convergence. Without this, a test
	// whose ticks straddle a real :00 boundary sees CurrentWindow advance
	// mid-run and mints a spurious next-window shard. 12:30 sits 30m from
	// either edge of the hour window. Set BEFORE LoadStateMachines so the
	// bucket SMs and shardDeps capture it (the clock is read at SM build).
	prov.clock = func() time.Time { return time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC) }
	// stubConfigurer stands in for the gRPC dial so configureOnHeartbeat
	// attaches an in-memory DuckDB shard. Set before LoadStateMachines so
	// shardSMDeps picks it up.
	prov.configurer = &stubConfigurer{}
	prov.SetStore(db)
	prov.LoadStateMachines()
	return prov, launcher
}

// drainLaunches expects n shard launches to be in flight, then releases
// them one at a time — waiting for each goroutine to fully finish (its
// store write included) before releasing the next, so the per-launch
// store writes never overlap on the single-connection test store. This
// exercises Target=n (one bucket SM coordinating n shard SMs) without the
// store concurrency the tx store can't survive.
//
// It gates on the launchInFlight atomic (set true when runLaunch starts,
// false by its terminal defer after SetShardLaunchResult) — a race-free
// signal, unlike reading the entry fields the goroutine mutates.
func drainLaunches(t *testing.T, prov *Provisioner, launcher *inProcLauncher, n int) {
	t.Helper()
	waitFor(t, func() bool { return inFlightLaunches(prov) == n }, "%d launches to park", n)
	for i := 0; i < n; i++ {
		select {
		case launcher.gate <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Fatalf("no launch goroutine parked to release (%d/%d)", i, n)
		}
		want := n - (i + 1)
		waitFor(t, func() bool { return inFlightLaunches(prov) == want }, "launch %d to settle", i)
	}
}

// inFlightLaunches counts shard SMs whose runLaunch goroutine is running.
// Reads only the launchInFlight atomic — safe to poll while a launch
// writes the store.
func inFlightLaunches(prov *Provisioner) int {
	n := 0
	for _, sh := range prov.registry.Entries() {
		if sh.launchInFlight.Load() {
			n++
		}
	}
	return n
}

// waitFor polls cond until true or a 2s deadline, then fails with a
// formatted message.
func waitFor(t *testing.T, cond func() bool, msgf string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for "+msgf, args...)
		}
		time.Sleep(time.Millisecond)
	}
}

// simulateHeartbeats drives one ProcessHeartbeat per LAUNCHING entry,
// mimicking each freshly-booted shard's first (unconfigured) heartbeat:
// empty ShardID, the launcher-stamped instance id, a live addr.
func simulateHeartbeats(prov *Provisioner) {
	for _, e := range prov.registry.Entries() {
		if e.State == StateLaunching && e.InstanceID != "" {
			prov.ProcessHeartbeat(store.ShardHeartbeat{
				ShardID:    "", // unconfigured — correlate by instance id
				InstanceID: e.InstanceID,
				Addr:       e.InstanceID + ":9000",
				State:      "ADOPTED",
			})
		}
	}
}

// TestProvisioner_Coordination_ScaleUpLaunchPromote drives the provisioner's
// tick + heartbeat path (white-box) with Target=2 — the case worth
// exercising: one bucket SM coordinating two shard SMs through the whole
// flow. An empty STASIS bucket scales up, the two shard SMs launch through
// the gated in-process launcher, simulated heartbeats promote both to
// INGESTING, and the bucket cascade settles back to STASIS at Target.
func TestProvisioner_Coordination_ScaleUpLaunchPromote(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 2, time.Hour)
	prov, launcher := newFSMTestProvisioner(t, db)
	ctx := context.Background()

	// Tick 1: STASIS → WARMING mints two shards (stripes 0,1) for the
	// current window (under-target fill) and kicks both launches, which
	// park on the gate.
	prov.check(ctx)
	prov.evalWG.Wait() // shard Evaluates run in the background pool
	drainLaunches(t, prov, launcher, 2)
	if got := launcher.launchCountSafe(); got != 2 {
		t.Fatalf("expected 2 launches for Target=2, got %d", got)
	}

	if launching := countByState(prov, StateLaunching); launching != 2 {
		t.Fatalf("expected 2 LAUNCHING shards after warming, got %d", launching)
	}

	// One heartbeat per launched shard promotes both to INGESTING.
	simulateHeartbeats(prov)
	if got := prov.registry.TotalIngesting(); got != 2 {
		t.Fatalf("expected 2 INGESTING shards after heartbeat promotion, got %d", got)
	}

	// Subsequent ticks settle the cascade: the WARMING gate sees both
	// incoming shards INGESTING → ROTATING; nothing older to drain →
	// STASIS. Each transition consumes a tick (Evaluate dispatches on the
	// state it read at entry), so drive a few and assert it lands.
	var st BucketState
	for i := 0; i < 5; i++ {
		prov.check(ctx)
		prov.evalWG.Wait() // shard Evaluates run in the background pool
		if st = bucketState(t, prov); st == BucketStasis {
			break
		}
	}
	if st != BucketStasis {
		t.Fatalf("bucket should settle in STASIS, got %s", st)
	}
	if got := prov.registry.TotalIngesting(); got != 2 {
		t.Fatalf("expected 2 INGESTING shards at steady state, got %d", got)
	}
	// No further launches once at Target.
	prov.check(ctx)
	prov.evalWG.Wait() // shard Evaluates run in the background pool
	if got := launcher.launchCountSafe(); got != 2 {
		t.Fatalf("steady state should not launch more shards, got %d total", got)
	}
}

// TestProvisioner_Coordination_RestartReconnect promotes a shard, then simulates a
// coordinator restart (fresh registry + provisioner over the same store).
// LoadFromStore rehydrates the INGESTING entry with a nil gRPC handle;
// the first post-restart heartbeat reattaches it via ObserveHeartbeat.
func TestProvisioner_Coordination_RestartReconnect(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 1, time.Hour)

	prov1, launcher1 := newFSMTestProvisioner(t, db)
	ctx := context.Background()
	prov1.check(ctx)
	prov1.evalWG.Wait() // shard Evaluates run in the background pool
	drainLaunches(t, prov1, launcher1, 1)
	simulateHeartbeats(prov1)
	if prov1.registry.TotalIngesting() != 1 {
		t.Fatalf("setup: expected 1 INGESTING, got %d", prov1.registry.TotalIngesting())
	}
	var shardID, instanceID string
	for _, e := range prov1.registry.Entries() {
		if e.State == StateIngesting {
			shardID, instanceID = e.ID, e.InstanceID
		}
	}

	// Restart: brand-new provisioner over the same store.
	prov2, _ := newFSMTestProvisioner(t, db)
	got := prov2.registry.Get(shardID)
	if got == nil {
		t.Fatalf("shard %s not restored after restart", shardID)
	}
	if got.State != StateIngesting {
		t.Fatalf("restored state: got %s want INGESTING", got.State)
	}
	if got.Live != nil {
		t.Fatal("restored entry should have a nil gRPC handle (LoadFromStore can't redial)")
	}

	// The shard's next heartbeat (now carrying its real id) reconnects.
	prov2.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    shardID,
		InstanceID: instanceID,
		Addr:       instanceID + ":9000",
		State:      "INGESTING",
	})
	if prov2.registry.Get(shardID).Live == nil {
		t.Fatal("handle should be reattached after the post-restart heartbeat")
	}
	if prov2.registry.TotalIngesting() != 1 {
		t.Fatalf("expected 1 INGESTING after reconnect, got %d", prov2.registry.TotalIngesting())
	}
}

// TestProvisioner_Coordination_FollowerRoutesToShardMintedAfterBoot pins the
// 2026-10-18 prod outage: a follower coordinator booted, then the leader
// minted and promoted the next window's cohort. The follower's
// RefreshFromStore only updated entries it already held, and
// ProcessHeartbeat drops heartbeats for unknown shard ids, so the new
// cohort never entered the follower's registry. Once the old cohort left
// INGESTING, every row the follower received was DLQ'd ("no shard serves
// this service") for two days.
func TestProvisioner_Coordination_FollowerRoutesToShardMintedAfterBoot(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 1, time.Hour)
	ctx := context.Background()

	// The follower boots first, before the shard exists. Built by hand
	// rather than with newFSMTestProvisioner: routing judges liveness by
	// the wall clock (isStale), so the follower must keep its default clock
	// instead of the fixture's pinned instant. It drives no FSMs.
	reg := NewShardRegistry()
	reg.SetStore(ShardStore{db})
	if err := reg.LoadFromStore(noReconstruct); err != nil {
		t.Fatalf("follower LoadFromStore: %v", err)
	}
	follower := NewProvisioner(reg, nil, ProvisionerConfig{PollInterval: time.Second})
	follower.configurer = &stubConfigurer{}
	follower.SetStore(db)
	follower.leading.Store(false)

	leader, launcher := newFSMTestProvisioner(t, db)
	leader.check(ctx)
	leader.evalWG.Wait() // shard Evaluates run in the background pool
	drainLaunches(t, leader, launcher, 1)
	simulateHeartbeats(leader)
	if leader.registry.TotalIngesting() != 1 {
		t.Fatalf("setup: leader expected 1 INGESTING, got %d", leader.registry.TotalIngesting())
	}
	var shardID, instanceID string
	for _, e := range leader.registry.Entries() {
		if e.State == StateIngesting {
			shardID, instanceID = e.ID, e.InstanceID
		}
	}

	org, err := db.FindOrCreateOrg("follower-test")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	ns, err := db.FindOrCreateNamespace(org.ID, "anterior")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if err := db.AddShardService(shardID, ns.ID, "django"); err != nil {
		t.Fatalf("stamp service: %v", err)
	}

	// The shard keeps beating; its row is what the follower's sweep reads.
	if err := db.RecordHeartbeat(&store.ShardHeartbeat{
		ShardID:    shardID,
		InstanceID: instanceID,
		Addr:       instanceID + ":9000",
		State:      "INGESTING",
	}); err != nil {
		t.Fatalf("record heartbeat: %v", err)
	}

	// The follower's provisioner tick, twice, in production order.
	for i := 0; i < 2; i++ {
		follower.adoptFromHeartbeats()
		follower.check(ctx)
	}

	if follower.IngestingShard(ns.ID, "django") == nil {
		e := follower.registry.Get(shardID)
		if e == nil {
			t.Fatalf("follower never adopted %s — every row it receives is DLQ'd", shardID)
		}
		e.mu.RLock()
		defer e.mu.RUnlock()
		t.Fatalf("follower has %s but won't route to it: state=%s live=%v stale=%v (last beat %v) services=%v",
			shardID, e.State, e.Live != nil, e.isStale(), e.LastHeartbeatAt, e.Services)
	}
}

// ── small helpers ───────────────────────────────────────────────

func (l *inProcLauncher) launchCountSafe() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.launched)
}

func countByState(prov *Provisioner, st ShardState) int {
	n := 0
	for _, e := range prov.registry.Entries() {
		if e.State == st {
			n++
		}
	}
	return n
}

func bucketState(t *testing.T, prov *Provisioner) BucketState {
	t.Helper()
	prov.smMu.RLock()
	defer prov.smMu.RUnlock()
	for _, b := range prov.bucketSMs {
		st, err := b.State(context.Background())
		if err != nil {
			t.Fatalf("bucket state: %v", err)
		}
		return st
	}
	t.Fatal("no bucket SM")
	return ""
}

// TestProcessHeartbeat_UndialableShardDoesNotPromote reproduces the
// new-AZ security-group incident: a freshly launched shard heartbeats fine
// (shard → coordinator works) but the coordinator can't dial it back on
// 9000 (coordinator → shard times out). The heartbeat promote path runs
// Configure as INGESTING's OnEntry, *after* stateless has already written
// the destination state — so a dial failure leaves the shard INGESTING
// with a nil Live handle. It then heartbeats forever (never isStale), the
// ingest router silently skips it (Live == nil), and it strands a Target
// slot doing nothing. An undialable shard must NOT be promoted: it should
// stay LAUNCHING and retry once the coordinator can reach it.
func TestProcessHeartbeat_UndialableShardDoesNotPromote(t *testing.T) {
	db := openSMTestStore(t)
	bucket := seedBucket(t, db, BucketStasis)

	const shardID, instanceID = "s-undialable", "i-abc"
	if err := (ShardStore{db}).SaveShard(&Shard{
		ID:          shardID,
		State:       StateLaunching,
		InstanceID:  instanceID,
		BucketID:    bucket.ID,
		Tier:        "small",
		WindowStart: time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:   time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed shard: %v", err)
	}

	reg := NewShardRegistry()
	reg.SetStore(ShardStore{db})
	if err := reg.LoadFromStore(func(*Shard) (shard.Shard, error) {
		return nil, errNoReconstruct
	}); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	prov := NewProvisioner(reg, &recordingLauncher{}, ProvisionerConfig{PollInterval: time.Second})
	prov.configurer = failingConfigurer{} // coordinator → shard:9000 always times out
	prov.SetStore(db)
	prov.LoadStateMachines()

	// The shard reports in (shard → coordinator path is fine).
	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    shardID,
		InstanceID: instanceID,
		Addr:       "172.31.129.96:9000",
	})

	entry := reg.Get(shardID)
	if entry == nil {
		t.Fatal("shard vanished from registry")
	}
	entry.mu.RLock()
	state, live := entry.State, entry.Live
	entry.mu.RUnlock()

	if live != nil {
		t.Fatalf("undialable shard has a live handle %v despite the dial failing", live)
	}
	if state == StateIngesting {
		t.Fatalf("undialable shard promoted to INGESTING with Live==nil — zombie cohort member that strands a Target slot; want it to stay %s until reachable", StateLaunching)
	}
}

// TestAlreadyConfiguredAs covers the promote-gate tolerance: a shard that's
// already configured as exactly the requested id is reachable and in the
// right state, so the dial path must treat it as success — but a different
// id or a genuine network error must not.
func TestAlreadyConfiguredAs(t *testing.T) {
	const id = "shard-20260525T0000-94563133"
	// The gRPC-wrapped error a pre-idempotent shard returns.
	grpcErr := fmt.Errorf("rpc error: code = Unknown desc = shard already configured as %s", id)

	if !alreadyConfiguredAs(grpcErr, id) {
		t.Fatal("same-id already-configured error must be tolerated")
	}
	if alreadyConfiguredAs(grpcErr, "shard-other") {
		t.Fatal("must not tolerate an already-configured error for a different shard id")
	}
	if alreadyConfiguredAs(fmt.Errorf(`dial tcp 172.31.128.129:9000: i/o timeout`), id) {
		t.Fatal("a real network error must not be tolerated")
	}
	if alreadyConfiguredAs(nil, id) {
		t.Fatal("nil error must not match")
	}
}
