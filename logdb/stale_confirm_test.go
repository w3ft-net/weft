package logdb

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeInstances answers whether an instance is gone, recording each ask.
type fakeInstances struct {
	mu    sync.Mutex
	gone  bool
	asked []string
}

func (f *fakeInstances) InstanceGone(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	return f.gone, nil
}

// silentFor builds an INGESTING shard on instance i-1 whose last heartbeat
// was d before the fixture clock (12:00), with inst as the EC2 check.
func silentFor(t *testing.T, d time.Duration, inst *fakeInstances) *Shard {
	t.Helper()
	sm, _, entry := makeFixtureShardSM(t, StateIngesting, func(deps *ShardDeps) { deps.Instances = inst })
	entry.EBSVolumeID = "vol-1"
	entry.InstanceID = "i-1"
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC).Add(-d)
	return sm
}

func evaluatedState(t *testing.T, sm *Shard) ShardState {
	t.Helper()
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	st, _ := sm.smState(context.Background())
	return st
}

// A shard silent past suspectStaleAfter whose instance EC2 reports gone
// (the chaos case: terminated) recovers now, not at the full threshold.
func TestStale_AGoneInstanceIsStaleAfterAShortSilence(t *testing.T) {
	inst := &fakeInstances{gone: true}
	sm := silentFor(t, suspectStaleAfter+5*time.Second, inst)
	if st := evaluatedState(t, sm); st != StateSuspended {
		t.Fatalf("state: %s, want SUSPENDED", st)
	}
}

// A silent shard whose instance is still running may be paused or cut off
// rather than dead, and recovery would force its volume away: it gets the
// full threshold.
func TestStale_ARunningInstanceGetsTheFullThreshold(t *testing.T) {
	inst := &fakeInstances{gone: false}
	sm := silentFor(t, suspectStaleAfter+5*time.Second, inst)
	if st := evaluatedState(t, sm); st != StateIngesting {
		t.Fatalf("state: %s, want INGESTING", st)
	}
	if len(inst.asked) == 0 || inst.asked[0] != "i-1" {
		t.Fatalf("EC2 asked about %v, want i-1", inst.asked)
	}
}

// Under suspectStaleAfter nobody asks EC2: missing one beat is normal.
func TestStale_AShortGapDoesNotAskEC2(t *testing.T) {
	inst := &fakeInstances{gone: true}
	sm := silentFor(t, suspectStaleAfter-5*time.Second, inst)
	if st := evaluatedState(t, sm); st != StateIngesting {
		t.Fatalf("state: %s, want INGESTING", st)
	}
	if len(inst.asked) != 0 {
		t.Fatalf("EC2 asked about %v after a short gap", inst.asked)
	}
}
