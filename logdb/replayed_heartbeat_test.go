//go:build test

package logdb

import (
	"fmt"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// replayFixture is a provisioner holding one shard in state, whose last
// counted beat was lastBeatAgo ago and whose dial always fails (the instance
// is dead), plus the configurer so a test can see whether a dial was made.
func replayFixture(t *testing.T, state ShardState, lastBeatAgo time.Duration) (*Provisioner, *fakeConfigurer) {
	t.Helper()
	fc := &fakeConfigurer{err: fmt.Errorf("dial tcp 10.0.0.1:9000: i/o timeout")}
	reg := NewShardRegistry()
	e := &Shard{
		ShardRow: ShardRow{
			ID:         "s-dead",
			State:      state,
			InstanceID: "i-dead",
			Addr:       "10.0.0.1:9000",
		},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "weft"}},
	}
	if lastBeatAgo > 0 {
		e.LastHeartbeatAt = time.Now().Add(-lastBeatAgo)
	}
	reg.Add(e)
	prov := &Provisioner{
		registry:   reg,
		config:     ProvisionerConfig{PollInterval: time.Second, WindowDuration: 7 * 24 * time.Hour},
		configurer: fc,
	}
	prov.leading.Store(true) // promoteOnHeartbeat is leader-only; NewProvisioner sets this
	return prov, fc
}

func dialedAddr(fc *fakeConfigurer) string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.dialAddr
}

// The catch-up sweep re-delivers an instance's last beat for a minute after
// it dies. For a SUSPENDED shard that beat is the dead instance's own, so it
// passes the instance check, and promoteOnHeartbeat dialed its address with
// the provisioner's one ticker goroutine (and heartbeatMu) held. The dial to a
// dead IP ran for its full timeout; the FSM tick behind it, and with it the
// shard's reap-and-relaunch, did not run, until the leader's deadman gave up
// the lease. Seen in prod on 2026-10-26: 24.7s with no tick, then a
// "not promoting" line the instant the dial gave up.
func TestProcessHeartbeat_ASuspendedShardsReplayedBeatIsNotDialed(t *testing.T) {
	prov, fc := replayFixture(t, StateSuspended, 40*time.Second)

	prov.ProcessHeartbeat(shardstore.ShardHeartbeat{
		ShardID: "s-dead", InstanceID: "i-dead", Addr: "10.0.0.1:9000",
		Age: 40 * time.Second, // the beat the shard already counted, replayed
	})

	if got := dialedAddr(fc); got != "" {
		t.Fatalf("dialed %s for a beat the shard had already counted", got)
	}
}

// The rescue path stays: a SUSPENDED shard whose instance was only briefly
// unreachable comes back on the next real beat.
func TestProcessHeartbeat_ASuspendedShardsNewerBeatIsStillDialed(t *testing.T) {
	prov, fc := replayFixture(t, StateSuspended, 40*time.Second)

	prov.ProcessHeartbeat(shardstore.ShardHeartbeat{
		ShardID: "s-dead", InstanceID: "i-dead", Addr: "10.0.0.1:9000",
		Age: 2 * time.Second, // recorded 38s after the last one counted
	})

	if got := dialedAddr(fc); got != "10.0.0.1:9000" {
		t.Fatalf("dialed %q, want the beat's address: a real beat must still be able to rescue the shard", got)
	}
}

// A replacement's first beat is what promotes a LAUNCHING shard, and the shard
// has never counted a beat from it.
func TestProcessHeartbeat_ALaunchingShardsFirstBeatIsDialed(t *testing.T) {
	prov, fc := replayFixture(t, StateLaunching, 0)

	prov.ProcessHeartbeat(shardstore.ShardHeartbeat{
		ShardID: "s-dead", InstanceID: "i-dead", Addr: "10.0.0.1:9000",
	})

	if got := dialedAddr(fc); got != "10.0.0.1:9000" {
		t.Fatalf("dialed %q, want the beat's address", got)
	}
}
