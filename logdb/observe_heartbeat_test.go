//go:build test

package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// observeFixture is an INGESTING shard whose clock the test controls, wired
// to a registry over a fake store so TouchHeartbeat writes can be counted.
func observeFixture(t *testing.T) (*Shard, *fakeRegistryStore, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	_, _, entry := makeFixtureShardSM(t, StateIngesting, func(d *ShardDeps) {
		d.Clock = func() time.Time { return now }
		d.Registry = reg
	})
	fake.records["s-test"] = entry.persistedCopy()
	return entry, fake, &now
}

// The catch-up sweep replays beats up to a minute old. Stamping Clock() for
// them made a dead shard's last beat look fresh for as long as it stayed in
// the lookback window, so a follower coordinator could keep routing to it.
func TestObserveHeartbeat_StampsTheBeatNotTheReplay(t *testing.T) {
	entry, _, now := observeFixture(t)

	entry.ObserveHeartbeat(context.Background(), store.ShardHeartbeat{ShardID: "s-test", InstanceID: "i-1", Age: 50 * time.Second})

	want := now.Add(-50 * time.Second)
	if !entry.LastHeartbeatAt.Equal(want) {
		t.Fatalf("LastHeartbeatAt = %v, want %v (the beat's own time, 50s before the replay)", entry.LastHeartbeatAt, want)
	}
}

func TestObserveHeartbeat_NeverMovesLivenessBackwards(t *testing.T) {
	entry, _, now := observeFixture(t)

	entry.ObserveHeartbeat(context.Background(), store.ShardHeartbeat{ShardID: "s-test", InstanceID: "i-1"})
	fresh := entry.LastHeartbeatAt
	if !fresh.Equal(*now) {
		t.Fatalf("a beat just received over RPC should stamp the clock, got %v", fresh)
	}

	// A replay of an older beat (the same one, seen again by the sweep) must
	// not pull the stamp back.
	*now = now.Add(20 * time.Second)
	entry.ObserveHeartbeat(context.Background(), store.ShardHeartbeat{ShardID: "s-test", InstanceID: "i-1", Age: 20 * time.Second})
	if !entry.LastHeartbeatAt.Equal(fresh) {
		t.Fatalf("LastHeartbeatAt moved from %v to %v on a replay of the same beat", fresh, entry.LastHeartbeatAt)
	}
}

// The sweep re-delivers the beat the Heartbeat RPC already recorded. Writing
// shard_records again for it is a pure duplicate.
func TestObserveHeartbeat_SkipsThePersistWhenNothingIsNewer(t *testing.T) {
	entry, fake, now := observeFixture(t)
	beat := store.ShardHeartbeat{ShardID: "s-test", InstanceID: "i-1", RowCount: 100, DiskBytes: 5}

	entry.ObserveHeartbeat(context.Background(), beat)
	if fake.touches != 1 {
		t.Fatalf("first beat: %d persists, want 1", fake.touches)
	}
	if rec := fake.records["s-test"]; rec.DiskBytes != 5 || rec.RowCount != 100 {
		t.Fatalf("persisted facts = disk %d rows %d, want 5 and 100 (steady-state beats must carry the footprint)", rec.DiskBytes, rec.RowCount)
	}

	// Same beat replayed 5s later by the sweep: same instant, same facts.
	*now = now.Add(5 * time.Second)
	replay := beat
	replay.Age = 5 * time.Second
	entry.ObserveHeartbeat(context.Background(), replay)
	if fake.touches != 1 {
		t.Fatalf("replay of an already-recorded beat persisted again (%d writes)", fake.touches)
	}

	// A genuinely newer beat advances liveness and is written.
	*now = now.Add(5 * time.Second)
	entry.ObserveHeartbeat(context.Background(), beat)
	if fake.touches != 2 {
		t.Fatalf("a newer beat: %d persists, want 2", fake.touches)
	}

	// Same instant but the shard reports different facts: still written.
	changed := beat
	changed.RowCount = 200
	entry.ObserveHeartbeat(context.Background(), changed)
	if fake.touches != 3 {
		t.Fatalf("a beat with a new row_count: %d persists, want 3", fake.touches)
	}
}
