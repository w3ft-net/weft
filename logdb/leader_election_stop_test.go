package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// A leader that is stopped hands the lease back before LeaderElectionDone
// closes, so a process that waits on it before exiting leaves its successor a
// free lease and not a whole LeaseTTL of waiting.
func TestLeaderElection_StoppingTheLeaderReleasesTheLeaseBeforeDone(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetShardFSMLease(t, db)

	a := electorTestProvisioner(db, "coord-a", time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	a.startLeaderElection(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !a.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.IsLeader() {
		t.Fatal("coord-a never took the lease")
	}

	cancel()
	select {
	case <-a.LeaderElectionDone():
	case <-time.After(5 * time.Second):
		t.Fatal("the election loop did not stop")
	}

	b := electorTestProvisioner(db, "coord-b", time.Minute)
	b.leaderTick()
	if !b.IsLeader() {
		t.Fatal("coord-b did not get the lease straight away: the stopped leader's release had not landed")
	}
}

// With no election running there is nothing to wait for.
func TestLeaderElectionDone_IsClosedWhenNoElectionRuns(t *testing.T) {
	p := NewProvisioner(NewShardRegistry(), nil, ProvisionerConfig{})
	select {
	case <-p.LeaderElectionDone():
	default:
		t.Fatal("LeaderElectionDone blocks with no election running")
	}
}
