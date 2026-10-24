//go:build test

package logdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/internal/logtest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── fakes for the volume move ──────────────────────────────────────

// swapVolumes is a VolumeManager that tracks which instance each shard's
// volume is attached to, the one fact the swap phases re-derive from.
type swapVolumes struct {
	fakeVolumeManager
	mu         sync.Mutex
	volume     string // the shard's volume id
	shardID    string
	attachedTo string
	detachErr  error
	detaches   []bool // force flag of each detach
}

func (v *swapVolumes) FindByShard(_ context.Context, shardID string) (*VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if shardID != v.shardID {
		return nil, nil
	}
	st := "available"
	if v.attachedTo != "" {
		st = "in-use"
	}
	return &VolumeInfo{VolumeID: v.volume, State: st, InstanceID: v.attachedTo, ShardID: shardID}, nil
}

func (v *swapVolumes) AttachToInstance(_ context.Context, volumeID, instanceID, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.attachedTo != "" && v.attachedTo != instanceID {
		return fmt.Errorf("VolumeInUse: %s is attached to %s", volumeID, v.attachedTo)
	}
	v.attachedTo = instanceID
	return nil
}

func (v *swapVolumes) DetachFromInstance(_ context.Context, _ string, force bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.detaches = append(v.detaches, force)
	if v.detachErr != nil && !force {
		return v.detachErr
	}
	v.attachedTo = ""
	return nil
}

func (v *swapVolumes) where() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attachedTo
}

// fakePauser records the leader's Pause/Resume calls by instance address.
type fakePauser struct {
	mu        sync.Mutex
	pauses    []string // "addr→successor"
	resumes   []string
	pauseErr  map[string]error
	resumeErr map[string]error
}

func (p *fakePauser) Pause(_ context.Context, addr, successor string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pauses = append(p.pauses, addr+"→"+successor)
	return p.pauseErr[addr]
}

func (p *fakePauser) Resume(_ context.Context, addr string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumes = append(p.resumes, addr)
	return p.resumeErr[addr]
}

func (p *fakePauser) resumed(addr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contains(p.resumes, addr)
}

// readyConfigurer answers the swap's probe: Configure on the successor
// succeeds only once the test marks that address ready (DuckDB open).
type readyConfigurer struct {
	mu    sync.Mutex
	ready map[string]bool
}

func (c *readyConfigurer) setReady(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready == nil {
		c.ready = map[string]bool{}
	}
	c.ready[addr] = true
}

func (c *readyConfigurer) Dial(_ context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready[addr] {
		return nil, shard.NotReadyError()
	}
	return &movableShard{failingShard: failingShard{id: req.ShardId}, addr: addr}, nil
}

// ── fixture ────────────────────────────────────────────────────────

type upgradeFixture struct {
	sh     *Shard
	db     *store.Store
	now    *time.Time
	vols   *swapVolumes
	pauser *fakePauser
	cfg    *readyConfigurer
	ec2    *fakeSpareEC2
	pool   *SparePool
	launch *fakeLauncher
	deps   func(*ShardDeps)
}

const (
	oldInstance = "i-old"
	oldAddr     = "10.0.0.1:9000"
	spareAddr   = "10.0.0.2:9000"
)

// newUpgradeFixture is a live shard in origin on i-old, its volume
// attached there, with one heartbeating current-image spare in its pair.
func newUpgradeFixture(t *testing.T, origin ShardState, withSpare bool) *upgradeFixture {
	t.Helper()
	prevProbe := swapProbeTimeout
	swapProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { swapProbeTimeout = prevProbe })
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	f := &upgradeFixture{now: &now, pauser: &fakePauser{}, cfg: &readyConfigurer{}, ec2: newFakeSpareEC2("ami-1"), launch: &fakeLauncher{}}
	clock := func() time.Time { return *f.now }
	f.vols = &swapVolumes{volume: "vol-1", shardID: "s-test", attachedTo: oldInstance}
	reg := NewShardRegistry()
	sh, db, _ := makeFixtureShardSM(t, origin, func(d *ShardDeps) {
		d.Clock = clock
		d.Registry = reg
		d.Volumes = f.vols
		d.Configurer = f.cfg
		d.Pauser = f.pauser
		d.Launcher = f.launch
		d.UpgradeRouteGrace = 2 * time.Minute
		f.pool = NewSparePool(ShardStore{d.Store.(ShardStore).Store}, f.ec2, clock)
		d.Spares = f.pool
	})
	f.sh, f.db = sh, db
	reg.mu.Lock()
	reg.entries[sh.ID] = sh
	reg.mu.Unlock()

	ss := ShardStore{db}
	if err := ss.SetShardLaunchResult(sh.ID, oldInstance, "us-east-2a", "vol-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetShardAddr(sh.ID, oldAddr); err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	sh.InstanceID, sh.Addr, sh.AZ, sh.EBSVolumeID = oldInstance, oldAddr, "us-east-2a", "vol-1"
	sh.LastHeartbeatAt = now
	sh.RowCount = 10
	sh.Services = []store.ShardService{{ShardID: sh.ID, NamespaceID: 1, Service: "svc"}}
	sh.Live = &movableShard{failingShard: failingShard{id: sh.ID}, addr: oldAddr}
	sh.mu.Unlock()

	if withSpare {
		f.addSpare(t, "i-spare", spareAddr)
	}
	return f
}

func (f *upgradeFixture) addSpare(t *testing.T, id, addr string) {
	t.Helper()
	ss := ShardStore{f.db}
	f.ec2.mu.Lock()
	f.ec2.instances[id] = &SpareInstance{InstanceID: id, Tier: "small", AZ: "us-east-2a", ImageID: "ami-1", LaunchedAt: *f.now}
	f.ec2.mu.Unlock()
	if err := ss.InsertSpare(&Spare{InstanceID: id, Tier: "small", AZ: "us-east-2a", ImageID: "ami-1", LaunchedAt: f.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := ss.ObserveSpareHeartbeat(id, addr, "sha-new", *f.now); err != nil {
		t.Fatal(err)
	}
}

func (f *upgradeFixture) evaluate(t *testing.T) {
	t.Helper()
	if err := f.sh.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

func (f *upgradeFixture) advance(d time.Duration) { *f.now = f.now.Add(d) }

// advanceAlive is advance with the serving instance heartbeating all the
// while, as a healthy one does every few seconds.
func (f *upgradeFixture) advanceAlive(d time.Duration) {
	f.advance(d)
	f.sh.mu.Lock()
	f.sh.LastHeartbeatAt = *f.now
	f.sh.mu.Unlock()
}

func (f *upgradeFixture) state(t *testing.T) ShardState {
	t.Helper()
	st, err := f.sh.smState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *upgradeFixture) phase() UpgradePhase {
	f.sh.mu.RLock()
	defer f.sh.mu.RUnlock()
	return f.sh.UpgradePhase
}

func (f *upgradeFixture) assertServing(t *testing.T, writable bool) {
	t.Helper()
	tr := TimeRange{Start: f.sh.WindowStart, End: f.sh.WindowEnd}
	holds, why := f.sh.queryable(tr, nil, *f.now)
	if !holds || why != "" {
		t.Fatalf("phase %q: queries routed away from the shard (holds=%v why=%q)", f.phase(), holds, why)
	}
	got := f.sh.ingestCandidate(1, "svc", *f.now) != nil
	if got != writable {
		t.Fatalf("phase %q: writable=%v, want %v", f.phase(), got, writable)
	}
}

func (f *upgradeFixture) terminated(id string) bool {
	return contains(f.launch.terminated, id) || contains(f.ec2.terminatedIDs(), id)
}

// terminations counts the launcher's terminate calls for id.
func (f *upgradeFixture) terminations(id string) int {
	f.launch.mu.Lock()
	defer f.launch.mu.Unlock()
	n := 0
	for _, t := range f.launch.terminated {
		if t == id {
			n++
		}
	}
	return n
}

// ── tests ──────────────────────────────────────────────────────────

// The whole upgrade: the shard is never routed away from, its volume moves
// to the spare, the demoted instance outlives its replacement spare's
// first heartbeat, and the shard returns to its origin state on the new
// instance.
func TestUpgrade_MovesTheVolumeToASpareWithoutLeavingRouting(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	ctx := context.Background()
	if err := f.sh.startUpgrade(ctx); err != nil {
		t.Fatalf("startUpgrade: %v", err)
	}
	if st := f.state(t); st != StateUpgrading || f.phase() != PhaseClaiming {
		t.Fatalf("after start: %s/%s", st, f.phase())
	}
	f.assertServing(t, true)

	// Claim, pause, and begin the swap in one tick; the spare isn't open yet.
	f.evaluate(t)
	if f.phase() != PhaseSwapping {
		t.Fatalf("phase after the first tick: %q, want swapping", f.phase())
	}
	if len(f.pauser.pauses) != 1 || f.pauser.pauses[0] != oldAddr+"→"+spareAddr {
		t.Fatalf("pauses: %v, want the old instance paused toward the spare", f.pauser.pauses)
	}
	if f.vols.where() != "i-spare" {
		t.Fatalf("volume attached to %q, want the spare", f.vols.where())
	}
	// The old instance went quiet when it paused: queries still route here,
	// to be held by the gate, rather than failing as stale.
	f.advance(90 * time.Second)
	f.assertServing(t, true)

	f.cfg.setReady(spareAddr)
	f.evaluate(t)
	if f.phase() != PhaseResumed {
		t.Fatalf("phase once the spare opened: %q, want resumed", f.phase())
	}
	rec, _, _ := ShardStore{f.db}.GetShard(f.sh.ID)
	if rec.InstanceID != "i-spare" || rec.Addr != spareAddr || f.sh.Addr != spareAddr {
		t.Fatalf("shard not moved: row instance=%q addr=%q, entry addr=%q", rec.InstanceID, rec.Addr, f.sh.Addr)
	}
	f.assertServing(t, true)

	// The demoted instance stays (rollback target, redirecting stragglers)
	// through the route-convergence grace and until a replacement spare
	// heartbeats.
	f.evaluate(t)
	if f.terminated(oldInstance) {
		t.Fatal("demoted instance terminated before every coordinator could route to the new address")
	}
	f.advanceAlive(3 * time.Minute)
	f.evaluate(t)
	if f.terminated(oldInstance) {
		t.Fatal("demoted instance terminated before its replacement spare heartbeat")
	}
	if err := f.pool.Reconcile(ctx, map[SparePair]bool{{"small", "us-east-2a"}: true}, func(Spare) SpareUse { return SpareAdopted }); err != nil {
		t.Fatal(err)
	}
	heartbeatAll(t, ShardStore{f.db})
	f.evaluate(t)
	if !f.terminated(oldInstance) {
		t.Fatal("demoted instance outlived the upgrade")
	}
	if st := f.state(t); st != StateIngesting || f.phase() != "" {
		t.Fatalf("after the upgrade: %s/%q, want INGESTING with no phase", st, f.phase())
	}
}

// A READ_ONLY shard upgrades back to READ_ONLY and takes no writes on the
// way.
func TestUpgrade_ReadOnlyOriginIsNeverWritable(t *testing.T) {
	f := newUpgradeFixture(t, StateReadOnly, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertServing(t, false)
	f.cfg.setReady(spareAddr)
	f.evaluate(t)
	f.assertServing(t, false)
	f.sh.deps.UpgradeRouteGrace = 0
	f.advanceAlive(upgradeReplacementBudget + time.Second) // past waiting for a replacement spare
	f.evaluate(t)
	if st := f.state(t); st != StateReadOnly {
		t.Fatalf("after the upgrade: %s, want READ_ONLY", st)
	}
}

// A spare that never opens the shard rolls the upgrade back: the volume
// returns to the demoted instance, which resumes, and the spare is
// terminated.
func TestUpgrade_ASpareThatNeverReadiesRollsBack(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	if f.phase() != PhaseSwapping {
		t.Fatalf("phase: %q", f.phase())
	}
	f.advance(upgradeSwapBudget + time.Second)
	f.evaluate(t)
	if f.phase() != PhaseRollingBack {
		t.Fatalf("phase past the swap budget: %q, want rolling_back", f.phase())
	}
	f.assertServing(t, true)
	for i := 0; i < 3 && f.state(t) == StateUpgrading; i++ {
		f.evaluate(t)
	}
	if st := f.state(t); st != StateIngesting {
		t.Fatalf("after rollback: %s, want INGESTING", st)
	}
	if f.vols.where() != oldInstance {
		t.Fatalf("volume on %q after rollback, want the old instance", f.vols.where())
	}
	if !f.pauser.resumed(oldAddr) {
		t.Fatal("the old instance was never resumed")
	}
	if !f.terminated("i-spare") {
		t.Fatal("the spare that never readied was kept")
	}
	if f.sh.Addr != oldAddr || f.sh.InstanceID != oldInstance {
		t.Fatalf("shard moved despite the rollback: %s on %s", f.sh.InstanceID, f.sh.Addr)
	}
	if held, _ := f.pool.Held(f.sh.ID); held != nil {
		t.Fatalf("rollback left a claim on %s", held.InstanceID)
	}
}

// An old instance that can't drain in time is resumed in place; the volume
// never moved.
func TestUpgrade_APauseThatNeverDrainsRollsBack(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	f.pauser.pauseErr = map[string]error{oldAddr: errors.New("drain 3 in-flight calls: context deadline exceeded")}
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	if f.phase() != PhasePausing {
		t.Fatalf("phase after a failed pause: %q, want pausing (retry)", f.phase())
	}
	f.advance(upgradePauseBudget + time.Second)
	for i := 0; i < 3 && f.state(t) == StateUpgrading; i++ {
		f.evaluate(t)
	}
	if st := f.state(t); st != StateIngesting {
		t.Fatalf("after rollback: %s", st)
	}
	if f.vols.where() != oldInstance || len(f.vols.detaches) != 0 {
		t.Fatalf("volume moved (%q, %d detaches) although the pause never completed", f.vols.where(), len(f.vols.detaches))
	}
	if !f.pauser.resumed(oldAddr) {
		t.Fatal("the paused old instance was never resumed")
	}
}

// With the pool empty the upgrade launches a spare on demand and waits
// for its heartbeat.
func TestUpgrade_EmptyPoolLaunchesASpareOnDemand(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, false)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	if len(f.ec2.launched) != 1 || f.phase() != PhaseClaiming {
		t.Fatalf("launched %v, phase %q: want one on-demand spare, still claiming", f.ec2.launched, f.phase())
	}
	if len(f.pauser.pauses) != 0 {
		t.Fatal("paused before the successor was up")
	}
	id := f.ec2.launched[0]
	if err := (ShardStore{f.db}).ObserveSpareHeartbeat(id, spareAddr, "sha", *f.now); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	if f.phase() != PhaseSwapping || f.vols.where() != id {
		t.Fatalf("after the on-demand spare heartbeat: phase %q, volume on %q", f.phase(), f.vols.where())
	}
}

// Every phase re-derives from the row after a leader change: a new
// leader's entry, loaded fresh from the store, finishes the upgrade.
func TestUpgrade_ADifferentLeaderFinishesIt(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // → swapping, spare not ready yet

	row, ok, err := ShardStore{f.db}.GetShard(f.sh.ID)
	if err != nil || !ok {
		t.Fatalf("reload: %v %v", ok, err)
	}
	if row.UpgradePhase != PhaseSwapping || row.UpgradeSuccessorInstanceID != "i-spare" || row.UpgradeDemotedInstanceID != oldInstance || row.UpgradeOrigin != StateIngesting {
		t.Fatalf("persisted upgrade facts: %+v", row)
	}
	row.ensureSM(f.sh.deps)
	f.sh = row
	f.cfg.setReady(spareAddr)
	f.evaluate(t)
	if f.phase() != PhaseResumed {
		t.Fatalf("new leader's phase: %q, want resumed", f.phase())
	}
}

// The first deploy of the seamless upgrade rolls shards whose binary has no
// Pause. Such an old instance is terminated, as the roll did before, and its
// volume (force-detached: the instance never unmounted) moves to the spare;
// the upgrade then finishes as usual, without terminating it a second time.
func TestUpgrade_APreUpgradeBinaryIsCutOverCold(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	f.pauser.pauseErr = map[string]error{oldAddr: status.Error(codes.Unimplemented, "unknown method Pause")}
	f.vols.detachErr = errors.New("IncorrectState: the instance never unmounted")
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	if f.phase() != PhaseSwapping {
		t.Fatalf("phase: %q, want swapping", f.phase())
	}
	if !f.terminated(oldInstance) {
		t.Fatal("old instance without Pause was not terminated")
	}
	if f.vols.where() != "i-spare" {
		t.Fatalf("volume attached to %q, want the spare", f.vols.where())
	}

	f.cfg.setReady(spareAddr)
	f.evaluate(t)
	if f.phase() != PhaseResumed {
		t.Fatalf("phase: %q, want resumed", f.phase())
	}
	f.advanceAlive(3 * time.Minute)
	if err := f.pool.Reconcile(context.Background(), map[SparePair]bool{{"small", "us-east-2a"}: true}, func(Spare) SpareUse { return SpareAdopted }); err != nil {
		t.Fatal(err)
	}
	heartbeatAll(t, ShardStore{f.db})
	f.evaluate(t)
	if st := f.state(t); st != StateIngesting || f.phase() != "" {
		t.Fatalf("after the cut-over: %s/%q, want INGESTING with no phase", st, f.phase())
	}
	if n := f.terminations(oldInstance); n != 1 {
		t.Fatalf("old instance terminated %d times, want once", n)
	}
}

// If the spare can't take a cold cut-over's volume, there is no old
// instance to roll back to: the upgrade is abandoned to recovery.
func TestUpgrade_AFailedColdCutOverGoesToRecovery(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	f.pauser.pauseErr = map[string]error{oldAddr: status.Error(codes.Unimplemented, "unknown method Pause")}
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // old instance terminated; → swapping, spare never readies
	f.advance(upgradeSwapBudget + time.Second)
	f.evaluate(t) // → rolling back
	f.evaluate(t)
	if st := f.state(t); st != StateIngesting || f.phase() != "" {
		t.Fatalf("after a failed cut-over: %s/%q, want the origin state for its stale check to recover", st, f.phase())
	}
	if f.pauser.resumed(oldAddr) {
		t.Fatal("resumed a terminated instance")
	}
	if !f.terminated("i-spare") {
		t.Fatal("the spare that never readied was kept")
	}
}

// A spare that opens while the swap is waiting on it is picked up in that
// same tick, not a provisioner tick later: callers are holding.
func TestUpgrade_SwapKeepsAskingTheSpareWithinTheTick(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // → swapping, spare not ready yet
	if f.phase() != PhaseSwapping {
		t.Fatalf("phase: %q, want swapping", f.phase())
	}

	swapProbeTimeout = 10 * time.Second
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.cfg.setReady(spareAddr)
	}()
	f.evaluate(t)
	if f.phase() != PhaseResumed {
		t.Fatalf("phase: %q, want resumed within the tick the spare opened in", f.phase())
	}
}

// The hold window runs from the first Pause to the spare serving, and is
// measured from the persisted pause time, so it comes out right even when
// a different leader completes the swap.
func TestUpgrade_LogsTheHoldWindowFromTheFirstPause(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // pause sent; → swapping, spare not ready yet

	row, ok, err := ShardStore{f.db}.GetShard(f.sh.ID)
	if err != nil || !ok {
		t.Fatalf("reload: %v %v", ok, err)
	}
	if row.UpgradePausedAt == nil {
		t.Fatal("pause time not persisted")
	}
	f.advance(20 * time.Second)
	row.ensureSM(f.sh.deps)
	f.sh = row
	f.cfg.setReady(spareAddr)
	out := logtest.Capture(func() { f.evaluate(t) })
	if f.phase() != PhaseResumed {
		t.Fatalf("phase: %q, want resumed", f.phase())
	}
	if !strings.Contains(out, "upgrade hold window 20s (moved volume") {
		t.Fatalf("no 20s hold window in the log:\n%s", out)
	}
}

// A rotation that lands mid-upgrade rotates the shard's origin: it stops
// taking writes now and returns to READ_ONLY.
func TestUpgrade_RotationDuringAnUpgradeLandsInReadOnly(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.sh.Fire(context.Background(), TriggerRotateRequested); err != nil {
		t.Fatalf("RotateRequested during UPGRADING: %v", err)
	}
	if st := f.state(t); st != StateUpgrading {
		t.Fatalf("rotation interrupted the upgrade: %s", st)
	}
	f.assertServing(t, false)
	f.cfg.setReady(spareAddr)
	f.evaluate(t)
	f.sh.deps.UpgradeRouteGrace = 0
	f.advanceAlive(upgradeReplacementBudget + time.Second)
	f.evaluate(t)
	if st := f.state(t); st != StateReadOnly {
		t.Fatalf("after the upgrade: %s, want READ_ONLY", st)
	}
}

// A heartbeat from the claimed spare before the swap completes must not
// move the shard to it, nor be taken as the old instance's liveness.
func TestUpgrade_OnlyTheServingInstancesHeartbeatCounts(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := NewProvisioner(f.sh.deps.Registry, nil, ProvisionerConfig{})
	p.sharedDeps = f.sh.deps
	p.depsOnce.Do(func() {})
	f.advance(30 * time.Second)
	p.ProcessHeartbeat(store.ShardHeartbeat{ShardID: f.sh.ID, InstanceID: "i-spare", Addr: spareAddr})
	if f.sh.Addr != oldAddr || f.sh.LastHeartbeatAt.Equal(*f.now) {
		t.Fatalf("the spare's early heartbeat was applied: addr=%s last=%v", f.sh.Addr, f.sh.LastHeartbeatAt)
	}
	p.ProcessHeartbeat(store.ShardHeartbeat{ShardID: f.sh.ID, InstanceID: oldInstance, Addr: oldAddr})
	if !f.sh.LastHeartbeatAt.Equal(*f.now) {
		t.Fatal("the serving instance's heartbeat during an upgrade was dropped")
	}
}

// The reconciler replaces the claimed spare as soon as the claim leaves
// the pair bare, which is usually while the swap is still running. That
// replacement is the one the demoted instance waits for: the upgrade
// finishes at the end of the route grace, not after a fallback timeout.
func TestUpgrade_AReplacementLaunchedDuringTheSwapCounts(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	ctx := context.Background()
	if err := f.sh.startUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // claimed, paused, swapping (spare not open yet)

	pair := map[SparePair]bool{{"small", "us-east-2a"}: true}
	if err := f.pool.Reconcile(ctx, pair, func(Spare) SpareUse { return SpareInUse }); err != nil {
		t.Fatal(err)
	}
	heartbeatAll(t, ShardStore{f.db})

	f.cfg.setReady(spareAddr)
	f.evaluate(t) // resumed
	f.advanceAlive(f.sh.deps.UpgradeRouteGrace + time.Second)
	f.evaluate(t)
	if st := f.state(t); st != StateIngesting {
		t.Fatalf("after the route grace with a heartbeating replacement: %s/%q, want INGESTING", st, f.phase())
	}
	if !f.terminated(oldInstance) {
		t.Fatal("demoted instance not terminated")
	}
}
