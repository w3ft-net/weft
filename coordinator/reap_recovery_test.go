package coordinator

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// A shard with a bound EBS volume must NOT be reaped when its
// heartbeat goes stale. The ASG health-check replaces the dead
// instance, the lifecycle hook reattaches the EBS volume to the
// replacement, and the replacement heartbeats with the same shard
// id — which the rebind path in adoptFromHeartbeats handles.
// Removing the registry entry on stale heartbeat would break that:
// rebind can't find the entry and the replacement comes up as a
// brand-new adoption with no window/apps metadata.
func TestReap_PreservesHotShardWithVolume(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-with-ebs",
		State:       StateIngesting,
		Addr:        "10.0.0.10:9000",
		InstanceID:  "i-dying",
		EBSVolumeID: "vol-live-data",
		CreatedAt:   time.Now(),
	})

	// Heartbeat store: shard-with-ebs has no recent heartbeat.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}

	launcher := &recordingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	// Registry entry must survive.
	got := reg.Get("shard-with-ebs")
	if got == nil {
		t.Fatal("shard-with-ebs was reaped despite having a bound EBS volume")
	}

	// Launcher must NOT be asked to terminate — ASG health check
	// handles the instance replacement, and a manual terminate from
	// the coordinator would race the ASG.
	if len(launcher.terminated) != 0 {
		t.Errorf("expected no TerminateInstance calls for EBS-backed shard, got %v",
			launcher.terminated)
	}
}

// Legacy shards without an EBS volume fall back to the pre-S5
// behavior: stale heartbeat → reap. Data was already lost with the
// instance; removing the registry entry just prevents the ASG from
// accumulating ghost shard rows.
func TestReap_LegacyShardWithoutVolume(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:         "shard-legacy",
		State:      StateIngesting,
		Addr:       "10.0.0.99:9000",
		InstanceID: "i-legacy",
		// No EBSVolumeID — pre-S5 shard.
		// Past the grace period so reap is eligible.
		CreatedAt: time.Now().Add(-10 * time.Minute),
	})

	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}

	launcher := &recordingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	if reg.Get("shard-legacy") != nil {
		t.Error("legacy shard without EBS volume should be reaped on stale heartbeat")
	}
	if len(launcher.terminated) != 1 {
		t.Errorf("expected 1 terminate call for legacy reap, got %d", len(launcher.terminated))
	}
}

// A freshly-christened shard has a registry entry for its minted id
// but the most recent heartbeat still carries the empty "unclaimed"
// id (the one that triggered christening). adoptFromHeartbeats and
// reapDeadShards run in the same tick; without a grace window, reap
// sees the new entry absent from the live-heartbeat map and kills
// it immediately. Seen 2026-04-18 in prod as:
//   provisioner: christened shard-... at ... volume=
//   provisioner: reaping dead shard shard-... (no heartbeat, no volume)
// happening in the same second.
func TestReap_GracePeriodOnFreshChristening(t *testing.T) {
	reg := NewShardRegistry()
	// Entry was just christened ~1s ago. No EBS volume on this
	// shard (dev/docker path; the prod path is covered by the
	// preserve-on-EBS test).
	reg.Add(&ShardEntry{
		ID:         "shard-just-christened",
		State:      StateIngesting,
		Addr:       "10.0.0.5:9000",
		InstanceID: "i-fresh",
		CreatedAt:  time.Now().Add(-1 * time.Second),
	})

	// The heartbeats table has the pre-christening empty-id ping
	// that triggered the whole thing — not the new shard id.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "", Addr: "10.0.0.5:9000", InstanceID: "i-fresh", State: "ADOPTED"},
		},
	}
	launcher := &recordingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config:   ProvisionerConfig{Store: mockStore},
	}
	prov.reapDeadShards()

	if reg.Get("shard-just-christened") == nil {
		t.Fatal("freshly-christened shard reaped — need a grace period on CreatedAt")
	}
	if len(launcher.terminated) != 0 {
		t.Errorf("expected no terminate calls during grace period, got %v", launcher.terminated)
	}
}

// After the grace period, a shard that's still not heartbeating with
// its real id DOES get reaped (nothing is heartbeating for it).
// This closes the loop: grace is a window, not a forever-skip.
func TestReap_GracePeriodExpires(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:        "shard-stale",
		State:     StateIngesting,
		Addr:      "10.0.0.5:9000",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	})

	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config:   ProvisionerConfig{Store: mockStore},
	}
	prov.reapDeadShards()

	if reg.Get("shard-stale") != nil {
		t.Error("shard past the grace period should be reaped when truly stale")
	}
}

// Preserved shard can be rebound when a new instance comes up with
// the same shard id (the post-recovery heartbeat). This is the
// combined behavior: reap-preserves → rebind-rewires.
func TestReap_PreservedShardIsRebindable(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-rebind",
		State:       StateIngesting,
		Shard:       &failingShard{id: "shard-rebind"},
		Addr:        "10.0.0.10:9000",
		InstanceID:  "i-dead",
		EBSVolumeID: "vol-data",
		CreatedAt:   time.Now(),
	})

	// Tick 1: shard is dead (no heartbeat). Reap must preserve.
	tick1Store := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "other", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config:   ProvisionerConfig{Store: tick1Store},
	}
	prov.reapDeadShards()

	if reg.Get("shard-rebind") == nil {
		t.Fatal("preserve step failed — shard reaped")
	}

	// Tick 2: replacement instance up, heartbeats with same id at
	// new addr. adoptFromHeartbeats should take the rebind branch.
	tick2Store := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-rebind", Addr: "10.0.0.20:9000", InstanceID: "i-replacement", State: "INGESTING"},
		},
	}
	prov.config.Store = tick2Store
	// adoptFromHeartbeats in the rebind arm creates a real GRPCShard
	// so we can't call it directly here without a live listener.
	// Verify the preservation invariant the rebind relies on:
	// EBSVolumeID survived the reap cycle and the entry still has
	// its window metadata.
	got := reg.Get("shard-rebind")
	if got.EBSVolumeID != "vol-data" {
		t.Errorf("EBSVolumeID lost: got %q", got.EBSVolumeID)
	}
	if got.State != StateIngesting {
		t.Errorf("State changed: got %s", got.State)
	}
}
