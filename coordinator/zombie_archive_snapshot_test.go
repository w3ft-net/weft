package coordinator

import (
	"errors"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// TestZombieArchive_SnapshotsBeforeGivingUp pins the data-recovery
// expectation around the rebind-timeout branch of reapDeadShards.
//
// 2026-05-03 incident: an INGESTING shard with 52,471 rows of recent
// ingest sat in the wipe-and-await-rebind state across a deploy
// where the replacement instance never re-bound. After
// rebindTimeout, the reaper transitioned the row to ARCHIVED — but
// did NOT invoke any snapshot. The volume's data, never exported to
// parquet, became unrecoverable. The shard_records row had no
// parquet_path because none of the cold-storage paths ran.
//
// What we want: before giving up, take a best-effort EBS snapshot
// tagged with the shard id. The data isn't in parquet yet, but the
// snapshot lets an operator restore the volume and re-export
// manually. Snapshot failure (e.g., the volume was already deleted
// by another reclaim) must not block the ARCHIVED transition — that
// would leave the entry zombied forever.
func TestZombieArchive_SnapshotsBeforeGivingUp(t *testing.T) {
	prev := rebindTimeout
	rebindTimeout = time.Nanosecond
	t.Cleanup(func() { rebindTimeout = prev })

	reg := NewShardRegistry()
	reg.SetStore(newFakeRegistryStore())

	const id = "shard-zombie-snap"
	const vol = "vol-with-unflushed-rows"
	reg.Add(&ShardEntry{
		ID:              id,
		State:           StateIngesting,
		Apps:            []string{"weft"},
		EBSVolumeID:     vol,
		WindowStart:     time.Now().Add(-2 * 24 * time.Hour),
		WindowEnd:       time.Now().Add(5 * 24 * time.Hour),
		CreatedAt:       time.Now().Add(-2 * 24 * time.Hour),
		LastHeartbeatAt: time.Now().Add(-30 * time.Minute), // wipe stamp, well past rebindTimeout
	})

	vm := &retiringVolumeManager{}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		volumes:  vm,
		config: ProvisionerConfig{
			Store:        &mockHeartbeatStore{heartbeats: []store.ShardHeartbeat{{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"}}},
			PollInterval: time.Second,
		},
	}

	prov.reapDeadShards()

	if len(vm.snapshotCalls) != 1 {
		t.Fatalf("expected exactly 1 SnapshotVolume call before zombie archive, got %d", len(vm.snapshotCalls))
	}
	got := vm.snapshotCalls[0]
	if got.VolumeID != vol {
		t.Errorf("snapshot VolumeID = %q, want %q", got.VolumeID, vol)
	}
	if got.ShardID != id {
		t.Errorf("snapshot ShardID = %q, want %q (snapshot tag is the only way an operator finds it)", got.ShardID, id)
	}

	// State must still flip to ARCHIVED — snapshot is best-effort
	// preservation, not a precondition.
	entry := reg.Get(id)
	if entry == nil {
		t.Fatal("entry vanished after reapDeadShards")
	}
	if entry.State != StateArchived {
		t.Errorf("state after zombie archive = %q, want ARCHIVED", entry.State)
	}

	// Detach + Delete must NOT run on the zombie path — the volume
	// might still be claimed by a parallel recovery branch, or
	// already gone. The proper retireVolume sequence belongs to the
	// graceful archive() path.
	if len(vm.detachCalls) != 0 {
		t.Errorf("zombie archive must not call DetachFromInstance; got %d calls", len(vm.detachCalls))
	}
	if len(vm.deleteCalls) != 0 {
		t.Errorf("zombie archive must not call DeleteVolume; got %d calls", len(vm.deleteCalls))
	}
}

// TestZombieArchive_SnapshotFailureStillTransitions: if SnapshotVolume
// errors (volume already gone, AWS hiccup, IAM glitch), the reaper
// must still mark the entry ARCHIVED. Otherwise the loop runs every
// tick forever — same zombie symptom we're trying to fix, just one
// layer up. Recovery is impossible at that point, but holding the
// shard slot is worse: it inflates IngestingCount and blocks the
// reconciler from minting a fresh LAUNCHING row.
func TestZombieArchive_SnapshotFailureStillTransitions(t *testing.T) {
	prev := rebindTimeout
	rebindTimeout = time.Nanosecond
	t.Cleanup(func() { rebindTimeout = prev })

	reg := NewShardRegistry()
	reg.SetStore(newFakeRegistryStore())

	const id = "shard-zombie-snapfail"
	reg.Add(&ShardEntry{
		ID:              id,
		State:           StateIngesting,
		Apps:            []string{"weft"},
		EBSVolumeID:     "vol-already-gone",
		WindowStart:     time.Now().Add(-2 * 24 * time.Hour),
		WindowEnd:       time.Now().Add(5 * 24 * time.Hour),
		CreatedAt:       time.Now().Add(-2 * 24 * time.Hour),
		LastHeartbeatAt: time.Now().Add(-30 * time.Minute),
	})

	vm := &retiringVolumeManager{snapshotErr: errors.New("InvalidVolume.NotFound: vol-already-gone does not exist")}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		volumes:  vm,
		config: ProvisionerConfig{
			Store:        &mockHeartbeatStore{heartbeats: []store.ShardHeartbeat{{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"}}},
			PollInterval: time.Second,
		},
	}

	prov.reapDeadShards()

	if len(vm.snapshotCalls) != 1 {
		t.Errorf("expected 1 SnapshotVolume attempt, got %d", len(vm.snapshotCalls))
	}
	entry := reg.Get(id)
	if entry == nil {
		t.Fatal("entry vanished")
	}
	if entry.State != StateArchived {
		t.Errorf("state after snapshot failure = %q, want ARCHIVED (don't leak the zombie)", entry.State)
	}
}

// TestZombieArchive_NoVolume_NoSnapshotAttempt: legacy entries (and
// dev fixtures) carry no EBSVolumeID. The zombie path shouldn't call
// SnapshotVolume on an empty id — the AWS API would 400, and the
// dev path doesn't even wire a VolumeManager.
func TestZombieArchive_NoVolume_NoSnapshotAttempt(t *testing.T) {
	prev := rebindTimeout
	rebindTimeout = time.Nanosecond
	t.Cleanup(func() { rebindTimeout = prev })

	reg := NewShardRegistry()
	reg.SetStore(newFakeRegistryStore())

	// Note: no EBSVolumeID, so this entry takes the "legacy" path
	// (no-volume reaping) instead of the rebind-timeout branch.
	// That path Removes the registry entry rather than archiving in
	// place — the zombie-snapshot fix shouldn't change it.
	const id = "shard-no-volume"
	reg.Add(&ShardEntry{
		ID:              id,
		State:           StateIngesting,
		Apps:            []string{"weft"},
		WindowStart:     time.Now().Add(-2 * 24 * time.Hour),
		WindowEnd:       time.Now().Add(5 * 24 * time.Hour),
		CreatedAt:       time.Now().Add(-2 * 24 * time.Hour),
		LastHeartbeatAt: time.Now().Add(-30 * time.Minute),
	})

	vm := &retiringVolumeManager{}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		volumes:  vm,
		config: ProvisionerConfig{
			Store:        &mockHeartbeatStore{heartbeats: []store.ShardHeartbeat{{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"}}},
			PollInterval: time.Second,
		},
	}

	prov.reapDeadShards()

	if len(vm.snapshotCalls) != 0 {
		t.Errorf("expected 0 SnapshotVolume calls with no volume bound, got %d", len(vm.snapshotCalls))
	}
}
