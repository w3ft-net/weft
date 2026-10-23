package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
)

// fakeConcurrentShardStore is a minimal, thread-safe shardRowStore fake.
// evaluateStateMachines's real store wiring (ShardStore over the
// coordinator's pooled *store.Store) is safe for concurrent access, but
// the package's other white-box tests intentionally use a single-
// connection tx-scoped test store that "panics under concurrent use"
// (see provisioner_coordination_test.go) — so a concurrency test needs
// its own fake rather than reaching for that fixture.
type fakeConcurrentShardStore struct {
	mu   sync.Mutex
	rows map[string]*Shard
}

func newFakeConcurrentShardStore() *fakeConcurrentShardStore {
	return &fakeConcurrentShardStore{rows: map[string]*Shard{}}
}

func (f *fakeConcurrentShardStore) seed(sh *Shard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[sh.ID] = sh.persistedCopy()
}

func (f *fakeConcurrentShardStore) GetShard(id string) (*Shard, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return nil, false, nil
	}
	return r.persistedCopy(), true, nil
}

func (f *fakeConcurrentShardStore) DeleteShard(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, id)
	return nil
}

func (f *fakeConcurrentShardStore) SetShardState(id string, state ShardState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.State = state
	}
	return nil
}

func (f *fakeConcurrentShardStore) SetShardAddr(id, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.Addr = addr
	}
	return nil
}

func (f *fakeConcurrentShardStore) SetShardIngestFailures(id string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.IngestFailures = n
	}
	return nil
}

func (f *fakeConcurrentShardStore) SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.InstanceID = instanceID
		r.AZ = az
		r.EBSVolumeID = volumeID
		r.StripeIndex = stripeIndex
	}
	return nil
}

func (f *fakeConcurrentShardStore) IncrementShardLaunchAttempts(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.LaunchAttempts++
	}
	return nil
}

func (f *fakeConcurrentShardStore) SetShardParquetPath(id, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.ParquetPath = path
	}
	return nil
}

func (f *fakeConcurrentShardStore) RecordArchiveStats(id string, stats []shard.CustomerStat) error {
	return nil
}

func (f *fakeConcurrentShardStore) ClearShardVolume(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.EBSVolumeID = ""
	}
	return nil
}

func (f *fakeConcurrentShardStore) ClearShardInstance(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		r.InstanceID = ""
	}
	return nil
}

func (f *fakeConcurrentShardStore) LatestPreRecoveryState(id string) (ShardState, error) {
	return "", nil
}

// The export-fact methods are never reached by the shards these tests drive
// (none is ARCHIVING with an export in flight); they only satisfy the interface.
func (f *fakeConcurrentShardStore) BeginExportAttempt(string, int, string, time.Time, int) (ExportClaim, error) {
	return ExportClaimLost, nil
}
func (f *fakeConcurrentShardStore) SetExportSnapshot(string, string) error { return nil }
func (f *fakeConcurrentShardStore) SetExportInstance(string, string) error { return nil }
func (f *fakeConcurrentShardStore) ClearExportFacts(string) error          { return nil }
func (f *fakeConcurrentShardStore) LatestShardDiskBytes(string) (int64, error) {
	return 0, nil
}

// sleepyLauncher is a ShardLaunchAPI whose Terminate blocks for `delay`
// before succeeding — standing in for a slow AWS call (e.g. a stuck
// ec2:TerminateInstances during SUSPENDED recovery) so a test can measure
// whether N shards' Evaluate calls run one at a time (sequential: total
// wall time ~= N*delay) or with bounded concurrency (total wall time ~=
// delay, independent of N). See docs/known-issues.md
// "evaluateStateMachines runs shards sequentially".
type sleepyLauncher struct {
	delay time.Duration
	mu    sync.Mutex
	calls int
}

func (l *sleepyLauncher) Launch(_ context.Context, spec LaunchSpec) (LaunchResult, error) {
	return LaunchResult{InstanceID: "i-fake", AZ: spec.AZ, VolumeID: "vol-fake"}, nil
}

func (l *sleepyLauncher) Terminate(_ context.Context, _ string) error {
	time.Sleep(l.delay)
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	return nil
}

func (l *sleepyLauncher) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestEvaluateStateMachines_ShardsConvergeConcurrently pins the fix for
// docs/known-issues.md "evaluateStateMachines runs shards sequentially,
// so one slow shard's latency is additive to the whole tick": N SUSPENDED
// shards, each of whose recovery-reap step (evaluateRecovery ->
// Launcher.Terminate) takes `delay`, must converge in one tick without
// the tick's total wall time scaling with N. Before the fix, the plain
// `for` loop over shards in evaluateStateMachines makes this test fail
// (elapsed ~= N*delay); the bounded-concurrency fix keeps it ~= delay
// regardless of N.
func TestEvaluateStateMachines_ShardsConvergeConcurrently(t *testing.T) {
	const n = 6
	const delay = 80 * time.Millisecond

	registry := NewShardRegistry()
	fakeStore := newFakeConcurrentShardStore()
	launcher := &sleepyLauncher{delay: delay}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s-suspended-%d", i)
		sh := &Shard{
			ID:          id,
			State:       StateSuspended,
			InstanceID:  "i-dead-" + id,
			AZ:          "us-east-2a",
			EBSVolumeID: "vol-" + id,
			Tier:        "small",
			CreatedAt:   clock(),
			WindowStart: clock().Add(-time.Hour),
			WindowEnd:   clock().Add(time.Hour),
		}
		fakeStore.seed(sh)

		deps := ShardDeps{
			Store:          fakeStore,
			Launcher:       launcher,
			Clock:          clock,
			StaleThreshold: 2 * time.Minute,
			BootedAt:       clock().Add(-2 * time.Hour),
		}
		sh.ensureSM(newShardDeps(deps))
		registry.Add(sh)
	}

	p := NewProvisioner(registry, nil, DefaultProvisionerConfig())
	p.clock = clock

	start := time.Now()
	p.evaluateStateMachines(context.Background())
	p.evalWG.Wait() // shard Evaluates run in the background pool
	elapsed := time.Since(start)

	if got := launcher.callCount(); got != n {
		t.Fatalf("Terminate calls: got %d, want %d (every SUSPENDED shard should reap in this tick)", got, n)
	}

	// A sequential loop takes at least n*delay. Bounded concurrency should
	// land well under half that no matter how big n is. This threshold
	// fails against today's plain `for` loop and passes once shards
	// converge concurrently.
	sequential := time.Duration(n) * delay
	if elapsed >= sequential/2 {
		t.Fatalf("evaluateStateMachines took %v for %d shards at %v Terminate delay each (sequential lower bound %v) — looks like shards are evaluated one at a time instead of concurrently", elapsed, n, delay, sequential)
	}
}
