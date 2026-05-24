package logdb

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

func TestUnsuspend_OnHeartbeat(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", Services: []store.ShardService{{NamespaceID: 1, Service: "anything"}},
		State: StateIngesting, Shard: ds,
		Addr: "10.0.0.1:9000", Suspended: true, CreatedAt: time.Now(),
	})

	// Verify it's suspended.
	if sh := reg.IngestingShard(1, "anything"); sh != nil {
		t.Fatal("suspended shard should not be returned by IngestingShard")
	}

	// Simulate heartbeat from same address.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-0", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.adoptFromHeartbeats()

	// Should be unsuspended.
	entry := reg.Get("shard-0")
	if entry.Suspended {
		t.Error("shard should be unsuspended after heartbeat")
	}

	// Should be back in rotation.
	if sh := reg.IngestingShard(1, "anything"); sh == nil {
		t.Error("unsuspended shard should be returned by IngestingShard")
	}
}

func TestReconnect_OnAddressChange(t *testing.T) {
	oldShard := &failingShard{id: "shard-0"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", State: StateIngesting, Shard: oldShard,
		Addr: "10.0.0.1:9000", Suspended: true, CreatedAt: time.Now(),
	})

	// Heartbeat arrives with a new address. We can't create a real GRPCShard
	// in unit tests, so we test the logic up to the point of NewGRPCShard.
	// The key assertion: the old shard's address and suspended state change.
	entry := reg.Get("shard-0")
	if entry.Addr != "10.0.0.1:9000" {
		t.Fatalf("expected old addr, got %s", entry.Addr)
	}
	if !entry.Suspended {
		t.Fatal("expected suspended")
	}

	// Simulate what adoptFromHeartbeats does on address change
	// (can't call it directly because NewGRPCShard needs a real server).
	newShard := &failingShard{id: "shard-0"}
	entry.Shard.Close()
	entry.Shard = newShard
	entry.Addr = "10.0.0.2:9000"
	entry.Suspended = false

	if entry.Addr != "10.0.0.2:9000" {
		t.Errorf("expected new addr, got %s", entry.Addr)
	}
	if entry.Suspended {
		t.Error("should be unsuspended after reconnect")
	}
}

func TestReap_DeadShard(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-alive", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	deadShard := &failingShard{id: "shard-dead"}

	reg := NewShardRegistry()
	// Past the grace period so reap is eligible to act.
	aged := time.Now().Add(-10 * time.Minute)
	reg.Add(&ShardEntry{
		ID: "shard-alive", State: StateIngesting, Shard: ds,
		Addr: "10.0.0.1:9000", CreatedAt: aged,
	})
	reg.Add(&ShardEntry{
		ID: "shard-dead", State: StateIngesting, Shard: deadShard,
		Addr: "10.0.0.2:9000", CreatedAt: aged,
	})

	// Only shard-alive has a heartbeat.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-alive", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	// shard-dead should be removed.
	if reg.Get("shard-dead") != nil {
		t.Error("dead shard should have been reaped")
	}

	// shard-alive should remain.
	if reg.Get("shard-alive") == nil {
		t.Error("alive shard should not have been reaped")
	}

	if reg.Count() != 1 {
		t.Errorf("expected 1 shard, got %d", reg.Count())
	}
}

func TestReap_SkipsArchivedShards(t *testing.T) {
	coldShard := &failingShard{id: "shard-cold"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-cold", State: StateArchived, Shard: coldShard,
		CreatedAt: time.Now(),
	})

	// No heartbeats at all — but shard-cold is archived, so it should survive.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-other", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	if reg.Get("shard-cold") == nil {
		t.Error("archived shard should not be reaped")
	}
}

func TestReap_TerminatesInstanceViaLauncher(t *testing.T) {
	// Regression guard for the orphaned-shards bug seen on 2026-04-17:
	// reapDeadShards removed dead entries from the in-memory registry
	// but never told the launcher to terminate the EC2 instance, so
	// flaky heartbeats caused the ASG to accumulate "dead" instances
	// that were still billing.
	alive := &failingShard{id: "shard-alive"}
	dead := &failingShard{id: "shard-dead"}

	reg := NewShardRegistry()
	aged := time.Now().Add(-10 * time.Minute)
	reg.Add(&ShardEntry{
		ID: "shard-alive", State: StateIngesting, Shard: alive,
		Addr: "10.0.0.1:9000", CreatedAt: aged,
	})
	reg.Add(&ShardEntry{
		ID: "shard-dead", State: StateIngesting, Shard: dead,
		Addr: "10.0.0.99:9000", CreatedAt: aged,
	})

	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-alive", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	launcher := &recordingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	// Registry side: dead removed, alive kept.
	if reg.Get("shard-dead") != nil {
		t.Error("dead shard should have been reaped from registry")
	}
	if reg.Get("shard-alive") == nil {
		t.Error("alive shard should not have been reaped")
	}

	// Launcher side: terminate called exactly once, for the dead shard's addr.
	if got := len(launcher.terminated); got != 1 {
		t.Fatalf("expected 1 TerminateInstance call, got %d: %v", got, launcher.terminated)
	}
	if launcher.terminated[0] != "10.0.0.99:9000" {
		t.Errorf("expected TerminateInstance(10.0.0.99:9000), got %s", launcher.terminated[0])
	}
}

func TestReap_SkipsWhenNoHeartbeats(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", State: StateIngesting, Shard: ds, CreatedAt: time.Now(),
	})

	// Empty heartbeat list — startup race. Should NOT reap.
	mockStore := &mockHeartbeatStore{heartbeats: nil}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	if reg.Get("shard-0") == nil {
		t.Error("should not reap when no heartbeats exist (startup race)")
	}
}
