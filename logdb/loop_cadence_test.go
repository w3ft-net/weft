package logdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
)

// ARCHIVED and GONE shards have no instance and nothing to converge: the
// tick doesn't evaluate them.
func TestConvergingShards_SkipsTerminalShards(t *testing.T) {
	var all []*Shard
	for _, st := range []ShardState{StateIngesting, StateArchived, StateReadOnly, StateGone, StateArchiving} {
		all = append(all, &Shard{
			ShardRow: ShardRow{
				ID:    string(st),
				State: st,
			},
		})
	}
	var got []string
	for _, e := range convergingShards(all) {
		got = append(got, e.ID)
	}
	if fmt.Sprint(got) != "[INGESTING READ_ONLY ARCHIVING]" {
		t.Fatalf("evaluated %v", got)
	}
}

// The deadman's switch measures time, not ticks: a 5s tick must not make
// a leader give up its lease after 10s of one slow AWS call, and a stuck
// leader must give way before its lease would have expired.
func TestProgressStale_HasAFloorUnderFastTicks(t *testing.T) {
	p := &Provisioner{config: ProvisionerConfig{PollInterval: 5 * time.Second}}
	p.lastProgress.Store(time.Now().Add(-15 * time.Second).UnixNano())
	if p.progressStale() {
		t.Fatal("stale after 15s at a 5s tick")
	}
	p.lastProgress.Store(time.Now().Add(-progressStaleFloor - time.Second).UnixNano())
	if !p.progressStale() {
		t.Fatal("not stale past the floor")
	}
	if progressStaleFloor >= coordination.LeaseTTL {
		t.Fatalf("deadman floor %s is no faster than the lease TTL %s", progressStaleFloor, coordination.LeaseTTL)
	}
}

// A failed launch isn't retried until its backoff passes, so the retry
// budget spans time rather than ticks.
func TestShardSM_FailedLaunchWaitsOutItsBackoff(t *testing.T) {
	fl := &fakeLauncher{err: fmt.Errorf("InsufficientInstanceCapacity")}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) {
		d.Launcher = fl
		d.LaunchRetryBackoff = 300 * time.Millisecond
	})
	entry.AZ = "us-east-2a"
	ctx := context.Background()

	if err := sm.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := sm.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fl.launchCount(); n != 1 {
		t.Fatalf("launches within the backoff: %d, want 1", n)
	}
	waitForLaunch(t, sm)
	if err := sm.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	waitForLaunch(t, sm)
	if n := fl.launchCount(); n != 2 {
		t.Fatalf("launches after the backoff: %d, want 2", n)
	}
}

// An upgrade's demoted instance redirects for a few of the provisioner's
// own ticks, since followers repoint once a tick.
func TestShardDeps_RouteGraceFollowsTheTick(t *testing.T) {
	p := NewProvisioner(NewShardRegistry(), nil, ProvisionerConfig{PollInterval: 5 * time.Second})
	if got := p.shardDeps().UpgradeRouteGrace; got != routeGraceTicks*5*time.Second {
		t.Fatalf("route grace: %s", got)
	}
}
