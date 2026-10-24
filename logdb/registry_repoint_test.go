//go:build test

package logdb

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
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
	entry := &Shard{ID: "s-move", State: StateIngesting, Addr: "10.0.0.1:9000", BucketID: 1, Live: live}
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

	entry.ObserveHeartbeat(context.Background(), store.ShardHeartbeat{ShardID: "s-test", InstanceID: "i-2", Addr: "10.0.0.2:9000"})
	cfg.mu.Lock()
	dials := len(cfg.calls)
	cfg.mu.Unlock()
	if dials == 0 {
		t.Fatal("heartbeat from the new address left the handle on the old one")
	}
}
