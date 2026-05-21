package logdb

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// A shard with a bound EBS volume must NOT be reaped from the
// registry when its heartbeat goes stale — rebind needs the entry
// to persist across the heartbeat gap. The dead instance, however,
// IS explicitly terminated via TerminateForReplacement (with
// ShouldDecrementDesiredCapacity=false) so the ASG launches a
// replacement immediately rather than waiting for its own
// health check. InstanceID gets nulled on the entry after the
// terminate call so the op doesn't re-fire every tick; rebind
// will restore it from the replacement's heartbeat.
func TestReap_PreservesHotShardWithVolume(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-with-ebs",
		State:       StateIngesting,
		Addr:        "10.0.0.10:9000",
		InstanceID:  "i-dying",
		EBSVolumeID: "vol-live-data",
		// Past the grace period so we actually hit the
		// recovery-terminate branch.
		CreatedAt: time.Now().Add(-10 * time.Minute),
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

	// Coordinator explicitly asked ASG to replace the dead instance.
	if len(launcher.terminatedForReplace) != 1 || launcher.terminatedForReplace[0] != "i-dying" {
		t.Errorf("want 1 TerminateForReplacement call for i-dying, got %v",
			launcher.terminatedForReplace)
	}
	// Decrement-semantics TerminateInstance (for archive) must NOT
	// fire — that'd tell the ASG to shrink, which would lose us a
	// recovery slot.
	if len(launcher.terminated) != 0 {
		t.Errorf("TerminateInstance (decrement) must not fire on recovery path, got %v",
			launcher.terminated)
	}

	// InstanceID nulled so next tick doesn't re-fire the terminate.
	if got.InstanceID != "" {
		t.Errorf("InstanceID: got %q, want empty (nulled post-terminate)", got.InstanceID)
	}
	// EBSVolumeID preserved so rebind can find the volume.
	if got.EBSVolumeID != "vol-live-data" {
		t.Errorf("EBSVolumeID: got %q, want vol-live-data (must survive for rebind)", got.EBSVolumeID)
	}
}

// Idempotency: once the terminate request has been issued and
// InstanceID is nulled, subsequent reapDeadShards ticks during the
// recovery window (until the replacement heartbeats) must not
// re-fire the terminate. This prevents a storm of terminate calls
// across the 1-3 minute replacement interval.
func TestReap_DoesNotRetryTerminateOnceInstanceIDNulled(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-mid-recovery",
		State:       StateIngesting,
		Addr:        "10.0.0.10:9000",
		InstanceID:  "", // already nulled by a prior tick's terminate
		EBSVolumeID: "vol-live-data",
		CreatedAt:   time.Now().Add(-10 * time.Minute),
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
		config:   ProvisionerConfig{Store: mockStore},
	}
	prov.reapDeadShards()

	if len(launcher.terminatedForReplace) != 0 {
		t.Errorf("entry with empty InstanceID must not re-fire terminate; got %v",
			launcher.terminatedForReplace)
	}
	if reg.Get("shard-mid-recovery") == nil {
		t.Error("entry must stay in registry during recovery window")
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
