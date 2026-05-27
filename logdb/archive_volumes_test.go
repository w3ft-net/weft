package logdb

import (
	"context"
	"sync"
	"testing"
)

// retiringVolumeManager captures the snapshot→detach→delete sequence the
// shard FSM's retireVolume runs when an archiving shard has an
// ebs_volume_id.
type retiringVolumeManager struct {
	mu sync.Mutex

	snapshotCalls []SnapshotOpts
	detachCalls   []struct {
		VolumeID string
		Force    bool
	}
	deleteCalls []string

	snapshotErr error
	detachErr   error
	deleteErr   error
}

func (r *retiringVolumeManager) CreateForShard(_ context.Context, _ CreateVolumeOpts) (string, error) {
	return "", nil
}
func (r *retiringVolumeManager) FindByShard(_ context.Context, _ string) (*VolumeInfo, error) {
	return nil, nil
}
func (r *retiringVolumeManager) ListAvailableByTier(_ context.Context, _, _ string) ([]VolumeInfo, error) {
	return nil, nil
}
func (r *retiringVolumeManager) TagWithShardID(_ context.Context, _, _ string) error { return nil }
func (r *retiringVolumeManager) AttachToInstance(_ context.Context, _, _, _ string) error {
	return nil
}

func (r *retiringVolumeManager) SnapshotVolume(_ context.Context, opts SnapshotOpts) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshotCalls = append(r.snapshotCalls, opts)
	if r.snapshotErr != nil {
		return "", r.snapshotErr
	}
	return "snap-" + opts.ShardID, nil
}

func (r *retiringVolumeManager) DetachFromInstance(_ context.Context, volumeID string, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detachCalls = append(r.detachCalls, struct {
		VolumeID string
		Force    bool
	}{volumeID, force})
	return r.detachErr
}

func (r *retiringVolumeManager) DeleteVolume(_ context.Context, volumeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteCalls = append(r.deleteCalls, volumeID)
	return r.deleteErr
}

// shardWithVolumes builds a *Shard with only the deps retireVolume needs
// (the VolumeManager) — retireVolume is store-free, so this skips the full
// FSM fixture. The full archive sequence (export → retire → terminate →
// ARCHIVED) is covered in shardsm_test.go; these isolate the retire steps.
func shardWithVolumes(id string, vm VolumeManager) *Shard {
	sh := &Shard{ID: id}
	sh.deps = &ShardDeps{Volumes: vm}
	return sh
}

// retireVolume snapshots (with shard_id + parquet_path tags), force-detaches,
// then deletes the volume — in that order.
func TestRetireVolume_SnapshotThenDetachThenDelete(t *testing.T) {
	vm := &retiringVolumeManager{}
	sh := shardWithVolumes("shard-archive", vm)

	if err := sh.retireVolume(context.Background(), "vol-to-retire", "s3://weft/warp/shard-archive.parquet"); err != nil {
		t.Fatalf("retireVolume: %v", err)
	}

	// Snapshot first, with the right tags.
	if len(vm.snapshotCalls) != 1 {
		t.Fatalf("expected 1 SnapshotVolume, got %d", len(vm.snapshotCalls))
	}
	snap := vm.snapshotCalls[0]
	if snap.VolumeID != "vol-to-retire" {
		t.Errorf("snapshot VolumeID: got %q want vol-to-retire", snap.VolumeID)
	}
	if snap.ShardID != "shard-archive" {
		t.Errorf("snapshot ShardID: got %q want shard-archive", snap.ShardID)
	}
	if snap.ParquetPath != "s3://weft/warp/shard-archive.parquet" {
		t.Errorf("snapshot ParquetPath: got %q", snap.ParquetPath)
	}

	// Detach with force=true so a dead instance doesn't hang the call.
	if len(vm.detachCalls) != 1 {
		t.Fatalf("expected 1 DetachFromInstance, got %d", len(vm.detachCalls))
	}
	if !vm.detachCalls[0].Force {
		t.Error("detach should be force=true on archive")
	}

	// Delete the volume.
	if len(vm.deleteCalls) != 1 || vm.deleteCalls[0] != "vol-to-retire" {
		t.Errorf("expected 1 DeleteVolume for vol-to-retire, got %v", vm.deleteCalls)
	}
}

// A snapshot failure must abort before detach/delete and return an error —
// evaluateArchiving then leaves the volume in place (mid-cleanup) for the
// next tick to retry rather than losing data on an incomplete archive.
func TestRetireVolume_SnapshotFailureAborts(t *testing.T) {
	vm := &retiringVolumeManager{snapshotErr: errSnapshotBroken()}
	sh := shardWithVolumes("shard-snapfail", vm)

	err := sh.retireVolume(context.Background(), "vol-keep", "s3://weft/warp/shard-snapfail.parquet")
	if err == nil {
		t.Fatal("expected an error when the snapshot fails")
	}
	if len(vm.detachCalls) != 0 {
		t.Errorf("snapshot failed → must NOT detach, got %d", len(vm.detachCalls))
	}
	if len(vm.deleteCalls) != 0 {
		t.Errorf("snapshot failed → must NOT delete, got %d", len(vm.deleteCalls))
	}
}

func errSnapshotBroken() error { return snapErr{} }

type snapErr struct{}

func (snapErr) Error() string { return "snapshot simulated failure" }
