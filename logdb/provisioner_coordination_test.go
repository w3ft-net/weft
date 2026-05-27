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

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
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
	prov := NewProvisioner(reg, launcher, ProvisionerConfig{PollInterval: time.Second})
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
	seedDefaultBucket(t, db, nil, 2, time.Hour)
	prov, launcher := newFSMTestProvisioner(t, db)
	ctx := context.Background()

	// Tick 1: STASIS → WARMING mints two shards (stripes 0,1) for the
	// current window (under-target fill) and kicks both launches, which
	// park on the gate.
	prov.check(ctx)
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
	seedDefaultBucket(t, db, nil, 1, time.Hour)

	prov1, launcher1 := newFSMTestProvisioner(t, db)
	ctx := context.Background()
	prov1.check(ctx)
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
