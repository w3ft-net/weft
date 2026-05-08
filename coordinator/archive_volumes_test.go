package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// retiringVolumeManager captures the snapshot→detach→delete sequence
// that archive() should run when a shard has a cached ebs_volume_id.
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

// testArchiveIngestedShard sets up a DuckDBShard with a handful of
// rows and returns the entry, ready to be archived. Uses a real
// in-memory DuckDB so the export path has something to write.
func testArchiveIngestedShard(t *testing.T, id, volumeID string) *ShardEntry {
	t.Helper()
	ds, err := shard.NewDuckDBShard(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.Ingest([]*pb.LogRow{
		{Raw: "row 1", App: "vimgym", Customer: "k", TsUnixNs: time.Now().UnixNano()},
	}); err != nil {
		t.Fatal(err)
	}
	return &ShardEntry{
		ID:          id,
		State:       StateReadOnly,
		Shard:       ds,
		ParquetPath: "",
		EBSVolumeID: volumeID,
		Addr:        "10.0.0.1:9000",
		InstanceID:  "i-archive",
		CreatedAt:   time.Now().Add(-time.Hour),
	}
}

// On archive, after the Parquet export succeeds, the volume should
// be snapshotted (with shard_id + parquet_path tags), detached, and
// deleted. The registry row's ebs_volume_id should be cleared.
func TestArchive_SnapshotThenDetachThenDelete(t *testing.T) {
	vm := &retiringVolumeManager{}
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	entry := testArchiveIngestedShard(t, "shard-archive", "vol-to-retire")
	reg.Add(entry)

	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config: ProvisionerConfig{
			ParquetDir: t.TempDir(), // local path; no S3
		},
		volumes: vm,
	}

	prov.archive(entry)

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
	if snap.ParquetPath == "" {
		t.Error("snapshot ParquetPath should be set to the export path")
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

	// Registry-side: entry still in registry (archived), but
	// ebs_volume_id cleared so nothing else tries to reuse it.
	got := reg.Get("shard-archive")
	if got == nil {
		t.Fatal("archived entry missing from registry")
	}
	if got.State != StateArchived {
		t.Errorf("state: got %s want ARCHIVED", got.State)
	}
	if got.EBSVolumeID != "" {
		t.Errorf("EBSVolumeID should be cleared post-archive, got %q", got.EBSVolumeID)
	}

	// Store-side: persisted with empty ebs_volume_id.
	rec, ok := fake.get("shard-archive")
	if !ok {
		t.Fatal("store missing shard-archive")
	}
	if rec.EBSVolumeID != "" {
		t.Errorf("store EBSVolumeID: got %q want empty", rec.EBSVolumeID)
	}
	if rec.State != string(StateArchived) {
		t.Errorf("store State: got %q want ARCHIVED", rec.State)
	}
}

// A shard whose volume_id is empty (e.g. created under the old model
// before gap #4 rolled out) should archive cleanly without touching
// the VolumeManager.
func TestArchive_NoVolumeID_SkipsRetirement(t *testing.T) {
	vm := &retiringVolumeManager{}
	reg := NewShardRegistry()

	entry := testArchiveIngestedShard(t, "shard-novol", "") // no ebs_volume_id
	reg.Add(entry)

	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config: ProvisionerConfig{
			ParquetDir: t.TempDir(),
		},
		volumes: vm,
	}

	prov.archive(entry)

	if len(vm.snapshotCalls) != 0 {
		t.Errorf("no volume → no snapshot, got %d", len(vm.snapshotCalls))
	}
	if len(vm.detachCalls) != 0 {
		t.Errorf("no volume → no detach, got %d", len(vm.detachCalls))
	}
	if len(vm.deleteCalls) != 0 {
		t.Errorf("no volume → no delete, got %d", len(vm.deleteCalls))
	}

	got := reg.Get("shard-novol")
	if got == nil || got.State != StateArchived {
		t.Errorf("archive should still swap in ColdShard and transition state, got entry=%+v", got)
	}
}

// A snapshot failure must not proceed to delete — that would lose
// data if the archive turns out to have been incomplete.
func TestArchive_SnapshotFailureAbortsRetirement(t *testing.T) {
	vm := &retiringVolumeManager{snapshotErr: errSnapshotBroken()}
	reg := NewShardRegistry()

	entry := testArchiveIngestedShard(t, "shard-snapfail", "vol-keep")
	reg.Add(entry)

	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config: ProvisionerConfig{
			ParquetDir: t.TempDir(),
		},
		volumes: vm,
	}

	prov.archive(entry)

	if len(vm.detachCalls) != 0 {
		t.Errorf("snapshot failed → must NOT detach, got %d", len(vm.detachCalls))
	}
	if len(vm.deleteCalls) != 0 {
		t.Errorf("snapshot failed → must NOT delete, got %d", len(vm.deleteCalls))
	}

	// Entry should still be in the registry with the volume id
	// intact — operator can retry.
	got := reg.Get("shard-snapfail")
	if got == nil {
		t.Fatal("entry missing")
	}
	if got.EBSVolumeID != "vol-keep" {
		t.Errorf("EBSVolumeID: got %q want vol-keep (preserved for retry)", got.EBSVolumeID)
	}
}

func errSnapshotBroken() error { return snapErr{} }

type snapErr struct{}

func (snapErr) Error() string { return "snapshot simulated failure" }
