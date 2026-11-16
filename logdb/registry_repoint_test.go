//go:build test

package logdb

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	"github.com/w3ft-net/weft/shard"
)

// movableShard is a live handle that records where it points, like a
// shard.GRPCShard.
type movableShard struct {
	failingShard
	mu   sync.Mutex
	addr string
}

func (m *movableShard) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addr
}

func (m *movableShard) Home() string { return m.Addr() }

func (m *movableShard) Repoint(addr string) {
	m.mu.Lock()
	m.addr = addr
	m.mu.Unlock()
}

var _ shard.Shard = (*movableShard)(nil)

// A follower coordinator learns a shard moved instances only from its row:
// the shard's heartbeats go to whichever coordinator holds its stream. The
// refresh must move the follower's live handle along with the recorded
// addr, or the follower keeps writing to the old instance (2026-10-23:
// 634 batches DLQ'd that way after a roll).
func TestRefreshFromStore_RepointsALiveHandleWhoseShardMoved(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	live := &movableShard{failingShard: failingShard{id: "s-move"}, addr: "10.0.0.1:9000"}
	entry := &Shard{
		ShardRow: ShardRow{
			ID:       "s-move",
			State:    StateIngesting,
			Addr:     "10.0.0.1:9000",
			BucketID: 1,
		},
		Live: live,
	}
	reg.Add(entry)

	moved := entry.persistedCopy()
	moved.Addr = "10.0.0.2:9000"
	fake.records["s-move"] = moved

	reg.RefreshFromStore(true)
	if got := live.Addr(); got != "10.0.0.2:9000" {
		t.Fatalf("live handle still points at %s after the row moved to 10.0.0.2:9000", got)
	}
}

// The coordinator that does get the moved shard's heartbeat must not take
// the refreshed row addr as proof its handle is current: the handle is.
func TestObserveHeartbeat_RedialsWhenTheHandleIsBehindTheHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	cfg := &stubConfigurer{}
	_, _, entry := makeFixtureShardSM(t, StateIngesting, func(d *ShardDeps) {
		d.Clock = func() time.Time { return now }
		d.Registry = reg
		d.Configurer = cfg
	})
	entry.mu.Lock()
	entry.Addr = "10.0.0.2:9000" // already refreshed from the row
	entry.Live = &movableShard{failingShard: failingShard{id: "s-test"}, addr: "10.0.0.1:9000"}
	entry.mu.Unlock()

	entry.ObserveHeartbeat(context.Background(), shardstore.ShardHeartbeat{ShardID: "s-test", InstanceID: "i-2", Addr: "10.0.0.2:9000"})
	cfg.mu.Lock()
	dials := len(cfg.calls)
	cfg.mu.Unlock()
	if dials == 0 {
		t.Fatal("heartbeat from the new address left the handle on the old one")
	}
}

// redirectedShard is a live handle like shard.GRPCShard mid-pause: home is
// where the shard is recorded to live, cur is where calls go now, which a
// paused shard's redirect moves ahead of home. Addr reports cur.
type redirectedShard struct {
	failingShard
	mu        sync.Mutex
	home, cur string
	repointed []string
}

func (m *redirectedShard) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

func (m *redirectedShard) Home() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.home
}

func (m *redirectedShard) Repoint(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.home, m.cur = addr, addr
	m.repointed = append(m.repointed, addr)
}

// During a swap the old instance redirects calls to the standby while the
// row still names the old instance. The refresh must not read the redirect
// as a move and send the handle back to the paused instance: that undid the
// redirect on every tick of the 2026-10-24 roll ("moved to .38; repointed
// its handle" while calls were going through at .42).
func TestRefreshFromStore_LeavesAPauseRedirectAlone(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	live := &redirectedShard{failingShard: failingShard{id: "s-swap"}, home: "10.0.0.38:9000", cur: "10.0.0.42:9000"}
	entry := &Shard{
		ShardRow: ShardRow{
			ID:       "s-swap",
			State:    StateUpgrading,
			Addr:     "10.0.0.38:9000",
			BucketID: 1,
		},
		Live: live,
	}
	reg.Add(entry)
	fake.records["s-swap"] = entry.persistedCopy() // the row still names the old instance

	reg.RefreshFromStore(true)

	live.mu.Lock()
	defer live.mu.Unlock()
	if len(live.repointed) != 0 || live.cur != "10.0.0.42:9000" {
		t.Fatalf("refresh undid the pause redirect: repointed %v, calls now go to %s; want them left at 10.0.0.42:9000", live.repointed, live.cur)
	}
}

// staleListStore returns the rows as they were, then runs afterRead: the
// swap completing in the gap between the refresh's read and its apply.
type staleListStore struct {
	*fakeRegistryStore
	afterRead func()
}

func (s *staleListStore) ListShards() ([]*ShardRow, error) {
	rows, err := s.fakeRegistryStore.ListShards()
	if s.afterRead != nil {
		s.afterRead()
	}
	return rows, err
}

// RefreshFromStore reads the rows before it takes the registry lock. A
// swap that completes in between leaves the refresh holding the old
// instance's addr to apply over a handle that already moved; it must not
// send the handle back (the second "moved to .38" of the 2026-10-24 roll,
// after the shard was on .42).
func TestRefreshFromStore_ASwapCompletingMidReadIsNotUndone(t *testing.T) {
	fake := newFakeRegistryStore()
	st := &staleListStore{fakeRegistryStore: fake}
	reg := NewShardRegistry()
	reg.SetStore(st)
	live := &movableShard{failingShard: failingShard{id: "s-race"}, addr: "10.0.0.38:9000"}
	entry := &Shard{
		ShardRow: ShardRow{
			ID:       "s-race",
			State:    StateUpgrading,
			Addr:     "10.0.0.38:9000",
			BucketID: 1,
		},
		Live: live,
	}
	reg.Add(entry)
	fake.records["s-race"] = entry.persistedCopy() // the row the refresh reads: still the old instance

	st.afterRead = func() {
		// completeSwap: the row and the entry move to the successor.
		moved := entry.persistedCopy()
		moved.Addr = "10.0.0.42:9000"
		fake.records["s-race"] = moved
		entry.mu.Lock()
		entry.Addr = "10.0.0.42:9000"
		entry.locEpoch++ // as completeSwap does
		entry.mu.Unlock()
		live.Repoint("10.0.0.42:9000")
	}

	reg.RefreshFromStore(true)

	if got := live.Addr(); got != "10.0.0.42:9000" {
		t.Fatalf("a refresh that read the pre-swap row sent the handle back to %s; want it on 10.0.0.42:9000", got)
	}
}
