package logdb

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// TestProvisioner_RebindNilOldShardDoesNotPanic reproduces the
// 2026-04-30 production panic at provisioner.go:507. Sequence:
//
//  1. Coordinator restarts. LoadFromStore hydrates the registry with
//     entries for live (non-archived) shards but does NOT attach a
//     `shard.Shard` pointer — that field stays nil until a heartbeat
//     arrives and the heartbeat-adopt path attaches a fresh GRPCShard.
//
//  2. The shard's EC2 instance is replaced (instance refresh), so its
//     first heartbeat against the restarted coordinator carries the
//     shard's durable id but a NEW addr.
//
//  3. ProcessHeartbeat takes the rebind branch (existing != nil,
//     existing.Addr != hb.Addr). It dials the new addr, reassigns
//     `existing.Shard = gs`, and spawns a goroutine to defer-close
//     the old shard.
//
//  4. The deferred goroutine calls `oldShard.Close()` — but oldShard
//     is the nil that LoadFromStore left behind. Nil deref → panic.
//     Goroutine panic crashes the coordinator process; systemd
//     restarts it; the cycle repeats every time another shard's
//     instance gets refreshed. Registry never settles.
//
// The fix is a nil guard on oldShard before dispatching the deferred
// close. With the fix in place this test passes; without it the test
// binary crashes when the spawned goroutine fires.
func TestProvisioner_RebindNilOldShardDoesNotPanic(t *testing.T) {
	reg := NewShardRegistry()
	cfg := DevProvisionerConfig()
	cfg.CloseRebindGrace = 10 * time.Millisecond // fire the goroutine fast

	// Pre-populate the registry with the exact shape LoadFromStore
	// leaves for a live shard: durable id and addr, but no live
	// shard.Shard pointer yet. Mimics the post-restart state right
	// before the first heartbeat arrives.
	reg.Add(&ShardEntry{
		ID:          "shard-test-rebind-nil",
		Addr:        "172.31.128.70:9000",
		InstanceID:  "i-old",
		State:       StateIngesting,
		Apps:        []string{"weft/coordinator"},
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
		BucketID:    1,
		// Shard intentionally left nil — the LoadFromStore reconstruct
		// only attaches Shard for archived rows.
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })
	prov := NewProvisioner(reg, launcher, cfg)

	// First heartbeat for this id since the restart, at a different
	// addr. Forces the rebind branch in ProcessHeartbeat.
	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    "shard-test-rebind-nil",
		Addr:       "172.31.128.188:9000", // new addr post-instance-refresh
		InstanceID: "i-new",
		State:      "INGESTING",
	})

	// Wait past CloseRebindGrace so the deferred close goroutine has
	// fired. Under the bug the goroutine panics and the test binary
	// crashes here. Under the fix the goroutine returns cleanly.
	time.Sleep(100 * time.Millisecond)

	entry := reg.Get("shard-test-rebind-nil")
	if entry == nil {
		t.Fatal("entry disappeared after rebind")
	}
	if entry.Addr != "172.31.128.188:9000" {
		t.Errorf("Addr = %q, want the new addr 172.31.128.188:9000", entry.Addr)
	}
	if entry.InstanceID != "i-new" {
		t.Errorf("InstanceID = %q, want i-new", entry.InstanceID)
	}
}
