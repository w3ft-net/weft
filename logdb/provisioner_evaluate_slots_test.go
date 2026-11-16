package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// gateLauncher is a ShardLaunchAPI whose Terminate blocks indefinitely
// (until release is closed) for one designated instance — standing in
// for a stuck archive export or hung AWS call — and returns immediately
// for every other instance. It records how many Terminate calls for the
// stuck instance were ever in flight at once.
type gateLauncher struct {
	stuckInstance string
	release       chan struct{}

	mu         sync.Mutex
	terminated map[string]int
	stuckNow   int
	stuckMax   int
}

func (l *gateLauncher) Launch(_ context.Context, spec LaunchSpec) (LaunchResult, error) {
	return LaunchResult{InstanceID: "i-fake", AZ: spec.AZ, VolumeID: "vol-fake"}, nil
}

func (l *gateLauncher) Terminate(_ context.Context, instanceID string) error {
	l.mu.Lock()
	l.terminated[instanceID]++
	stuck := instanceID == l.stuckInstance
	if stuck {
		l.stuckNow++
		if l.stuckNow > l.stuckMax {
			l.stuckMax = l.stuckNow
		}
	}
	l.mu.Unlock()

	if stuck {
		<-l.release
		l.mu.Lock()
		l.stuckNow--
		l.mu.Unlock()
	}
	return nil
}

func (l *gateLauncher) terminatedCount(instanceID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.terminated[instanceID]
}

func (l *gateLauncher) stuckMaxInFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stuckMax
}

// TestEvaluateStateMachines_StuckShardOccupiesOnlyItsOwnSlot pins the
// slot model: MaxConcurrentEvaluate is the number of available
// concurrent evaluation slots, shared across ticks — not a per-tick
// fan-out the tick then waits on. One shard whose Evaluate never
// returns (stuck export) must cost exactly one slot and nothing else:
//
//   - the tick that dispatches it still returns promptly;
//   - later ticks keep converging every other shard on the remaining
//     slot(s);
//   - the stuck shard is never evaluated a second time while its first
//     Evaluate is still in flight.
//
// Before the fix, evaluateConcurrently ends in wg.Wait(), so the tick
// lasts as long as the slowest shard (archiveTimeout, 10 min in prod)
// and the other shards wait behind it.
func TestEvaluateStateMachines_StuckShardOccupiesOnlyItsOwnSlot(t *testing.T) {
	const fast = 3
	const stuckID = "s-stuck"

	registry := NewShardRegistry()
	fakeStore := newFakeConcurrentShardStore()
	launcher := &gateLauncher{
		stuckInstance: "i-dead-" + stuckID,
		release:       make(chan struct{}),
		terminated:    map[string]int{},
	}
	// Unblock the stuck Evaluate when the test ends so its goroutine
	// doesn't outlive the test (and so a failing run doesn't leak it).
	t.Cleanup(func() { close(launcher.release) })

	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }

	ids := []string{stuckID}
	for i := 0; i < fast; i++ {
		ids = append(ids, fmt.Sprintf("s-fast-%d", i))
	}
	for _, id := range ids {
		sh := &Shard{
			ShardRow: ShardRow{
				ID:          id,
				State:       StateSuspended,
				InstanceID:  "i-dead-" + id,
				AZ:          "us-east-2a",
				EBSVolumeID: "vol-" + id,
				Tier:        "small",
				CreatedAt:   clock(),
				WindowStart: clock().Add(-time.Hour),
				WindowEnd:   clock().Add(time.Hour),
			},
		}
		fakeStore.seed(sh)
		sh.ensureSM(newShardDeps(ShardDeps{
			Store:          fakeStore,
			Launcher:       launcher,
			Clock:          clock,
			StaleThreshold: 2 * time.Minute,
			BootedAt:       clock().Add(-2 * time.Hour),
		}))
		registry.Add(sh)
	}

	cfg := DefaultProvisionerConfig()
	cfg.MaxConcurrentEvaluate = 2 // one slot for the stuck shard, one left over
	p := NewProvisioner(registry, nil, cfg)
	p.clock = clock

	// tick runs one evaluateStateMachines and fails the test if it does
	// not return promptly — the barrier regression we are pinning.
	tick := func(n int) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			p.evaluateStateMachines(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("tick %d did not return within 1s while one shard's Evaluate is stuck — the tick is waiting on the slowest shard", n)
		}
	}

	// Keep ticking until every fast shard has been reaped (or we give
	// up). The stuck shard holds one of the two slots the whole time, so
	// this only converges if the other slot keeps cycling across ticks.
	deadline := time.Now().Add(3 * time.Second)
	for n := 1; ; n++ {
		tick(n)
		reaped := 0
		for _, id := range ids[1:] {
			if launcher.terminatedCount("i-dead-"+id) > 0 {
				reaped++
			}
		}
		if reaped == fast {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %d ticks only %d/%d fast shards converged; a stuck shard is starving the rest", n, reaped, fast)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := launcher.stuckMaxInFlight(); got != 1 {
		t.Fatalf("stuck shard had up to %d Evaluate(s) in flight at once, want exactly 1 — a shard must never be evaluated twice concurrently", got)
	}
}
