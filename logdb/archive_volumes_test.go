package logdb

import (
	"context"
	"sync"
	"testing"
)

// retiringVolumeManager captures the detach→delete sequence the shard
// FSM's retireVolume runs when an archiving shard has an ebs_volume_id.
type retiringVolumeManager struct {
	mu sync.Mutex

	detachCalls []struct {
		VolumeID string
		Force    bool
	}
	deleteCalls []string

	detachErr error
	deleteErr error
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

// retireVolume force-detaches, then deletes the volume — in that order.
// No snapshot is taken: the parquet export (Step 1) already made the
// data durable in S3, so the volume is disposable.
func TestRetireVolume_DetachThenDelete(t *testing.T) {
	vm := &retiringVolumeManager{}
	sh := shardWithVolumes("shard-archive", vm)

	if err := sh.retireVolume(context.Background(), "vol-to-retire"); err != nil {
		t.Fatalf("retireVolume: %v", err)
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

// A detach failure must abort before delete and return an error —
// evaluateArchiving then leaves the volume in place (mid-cleanup) for the
// next tick to retry rather than deleting a volume it never detached.
func TestRetireVolume_DetachFailureAborts(t *testing.T) {
	vm := &retiringVolumeManager{detachErr: errDetachBroken()}
	sh := shardWithVolumes("shard-detachfail", vm)

	err := sh.retireVolume(context.Background(), "vol-keep")
	if err == nil {
		t.Fatal("expected an error when detach fails")
	}
	if len(vm.deleteCalls) != 0 {
		t.Errorf("detach failed → must NOT delete, got %d", len(vm.deleteCalls))
	}
}

func errDetachBroken() error { return detachErr{} }

type detachErr struct{}

func (detachErr) Error() string { return "detach simulated failure" }
