package coordinator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// mockEC2Volumes captures each call made by the EC2VolumeManager so
// tests can assert both the arguments and the sequence. Each method
// returns canned responses; simulate errors by setting err* fields.
type mockEC2Volumes struct {
	mu sync.Mutex

	// CreateVolume
	createCalls     []*ec2.CreateVolumeInput
	createVolumeID  string
	createErr       error

	// DescribeVolumes: keyed by first filter's value joined with ","
	describeErr     error
	volumesByShard  map[string]ec2types.Volume // shard_id tag → volume
	volumesByID     map[string]ec2types.Volume // volume id → volume (for state polls)

	attachCalls     []*ec2.AttachVolumeInput
	attachErr       error

	detachCalls     []*ec2.DetachVolumeInput
	detachErr       error

	deleteCalls     []*ec2.DeleteVolumeInput
	deleteErr       error

	createTagCalls  []*ec2.CreateTagsInput
	createTagErr    error

	createSnapCalls []*ec2.CreateSnapshotInput
	createSnapID    string
	createSnapErr   error
}

func (m *mockEC2Volumes) CreateVolume(_ context.Context, in *ec2.CreateVolumeInput, _ ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, in)
	if m.createErr != nil {
		return nil, m.createErr
	}
	id := m.createVolumeID
	if id == "" {
		id = fmt.Sprintf("vol-auto-%d", len(m.createCalls))
	}
	// Remember for FindByShard.
	shardID := ""
	for _, t := range in.TagSpecifications {
		for _, tag := range t.Tags {
			if tag.Key != nil && *tag.Key == "shard_id" && tag.Value != nil {
				shardID = *tag.Value
			}
		}
	}
	if m.volumesByShard == nil {
		m.volumesByShard = make(map[string]ec2types.Volume)
	}
	if m.volumesByID == nil {
		m.volumesByID = make(map[string]ec2types.Volume)
	}
	vol := ec2types.Volume{
		VolumeId:         strPtr(id),
		AvailabilityZone: in.AvailabilityZone,
		State:            ec2types.VolumeStateAvailable,
	}
	if shardID != "" {
		m.volumesByShard[shardID] = vol
	}
	m.volumesByID[id] = vol
	return &ec2.CreateVolumeOutput{VolumeId: &id, State: ec2types.VolumeStateAvailable, AvailabilityZone: in.AvailabilityZone}, nil
}

func (m *mockEC2Volumes) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	// Respect volume-ids if given.
	if len(in.VolumeIds) > 0 {
		var out []ec2types.Volume
		for _, id := range in.VolumeIds {
			if v, ok := m.volumesByID[id]; ok {
				out = append(out, v)
			}
		}
		return &ec2.DescribeVolumesOutput{Volumes: out}, nil
	}
	// Respect tag:shard_id filter.
	for _, f := range in.Filters {
		if f.Name != nil && *f.Name == "tag:shard_id" && len(f.Values) > 0 {
			if v, ok := m.volumesByShard[f.Values[0]]; ok {
				return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{v}}, nil
			}
			return &ec2.DescribeVolumesOutput{}, nil
		}
	}
	return &ec2.DescribeVolumesOutput{}, nil
}

func (m *mockEC2Volumes) AttachVolume(_ context.Context, in *ec2.AttachVolumeInput, _ ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attachCalls = append(m.attachCalls, in)
	return &ec2.AttachVolumeOutput{}, m.attachErr
}

func (m *mockEC2Volumes) DetachVolume(_ context.Context, in *ec2.DetachVolumeInput, _ ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detachCalls = append(m.detachCalls, in)
	return &ec2.DetachVolumeOutput{}, m.detachErr
}

func (m *mockEC2Volumes) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, in)
	return &ec2.DeleteVolumeOutput{}, m.deleteErr
}

func (m *mockEC2Volumes) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createTagCalls = append(m.createTagCalls, in)
	return &ec2.CreateTagsOutput{}, m.createTagErr
}

func (m *mockEC2Volumes) CreateSnapshot(_ context.Context, in *ec2.CreateSnapshotInput, _ ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createSnapCalls = append(m.createSnapCalls, in)
	if m.createSnapErr != nil {
		return nil, m.createSnapErr
	}
	id := m.createSnapID
	if id == "" {
		id = fmt.Sprintf("snap-auto-%d", len(m.createSnapCalls))
	}
	return &ec2.CreateSnapshotOutput{SnapshotId: &id, State: ec2types.SnapshotStateCompleted}, nil
}

func (m *mockEC2Volumes) DescribeSnapshots(_ context.Context, in *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	// Completed is our default; individual tests can override by
	// using a canned response. For now, "any snapshot the test knows
	// about" is assumed completed.
	var out []ec2types.Snapshot
	for _, id := range in.SnapshotIds {
		id := id
		out = append(out, ec2types.Snapshot{SnapshotId: &id, State: ec2types.SnapshotStateCompleted})
	}
	return &ec2.DescribeSnapshotsOutput{Snapshots: out}, nil
}

// ── Tests ───────────────────────────────────────────────────

func TestVolumeManager_CreateForShard_TagsAndSizesVolume(t *testing.T) {
	mock := &mockEC2Volumes{}
	vm := NewEC2VolumeManagerWithClient(mock)

	id, err := vm.CreateForShard(context.Background(), CreateVolumeOpts{
		ShardID:   "shard-2026w16-small-01",
		Tier:      "small",
		AZ:        "us-east-2a",
		SizeGB:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a volume id, got empty")
	}

	if len(mock.createCalls) != 1 {
		t.Fatalf("expected 1 CreateVolume call, got %d", len(mock.createCalls))
	}
	call := mock.createCalls[0]

	if call.AvailabilityZone == nil || *call.AvailabilityZone != "us-east-2a" {
		t.Errorf("AZ: got %v want us-east-2a", call.AvailabilityZone)
	}
	if call.Size == nil || *call.Size != 100 {
		t.Errorf("Size: got %v want 100", call.Size)
	}
	if call.VolumeType != ec2types.VolumeTypeGp3 {
		t.Errorf("VolumeType: got %q want gp3", call.VolumeType)
	}

	// Tags must carry shard_id and shard_tier.
	tags := collectTags(call.TagSpecifications)
	if tags["shard_id"] != "shard-2026w16-small-01" {
		t.Errorf("tag shard_id: got %q", tags["shard_id"])
	}
	if tags["shard_tier"] != "small" {
		t.Errorf("tag shard_tier: got %q", tags["shard_tier"])
	}
}

func TestVolumeManager_FindByShard_ReturnsVolume(t *testing.T) {
	mock := &mockEC2Volumes{
		volumesByShard: map[string]ec2types.Volume{
			"shard-x": {
				VolumeId:         strPtr("vol-123"),
				AvailabilityZone: strPtr("us-east-2b"),
				State:            ec2types.VolumeStateAvailable,
			},
		},
	}
	vm := NewEC2VolumeManagerWithClient(mock)

	got, err := vm.FindByShard(context.Background(), "shard-x")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a volume, got nil")
	}
	if got.VolumeID != "vol-123" {
		t.Errorf("VolumeID: got %q want vol-123", got.VolumeID)
	}
	if got.AZ != "us-east-2b" {
		t.Errorf("AZ: got %q want us-east-2b", got.AZ)
	}
}

func TestVolumeManager_FindByShard_NoneReturnsNil(t *testing.T) {
	mock := &mockEC2Volumes{}
	vm := NewEC2VolumeManagerWithClient(mock)

	got, err := vm.FindByShard(context.Background(), "shard-unknown")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestVolumeManager_AttachToInstance(t *testing.T) {
	mock := &mockEC2Volumes{}
	// Pre-register the volume so WaitForVolumeAttached can observe it
	// transitioning to in-use.
	mock.volumesByID = map[string]ec2types.Volume{
		"vol-1": {
			VolumeId: strPtr("vol-1"),
			State:    ec2types.VolumeStateInUse,
		},
	}
	vm := NewEC2VolumeManagerWithClient(mock)
	vm.pollInterval = time.Millisecond
	vm.pollAttempts = 3

	if err := vm.AttachToInstance(context.Background(), "vol-1", "i-1", "/dev/sdf"); err != nil {
		t.Fatal(err)
	}
	if len(mock.attachCalls) != 1 {
		t.Fatalf("expected 1 AttachVolume call, got %d", len(mock.attachCalls))
	}
	a := mock.attachCalls[0]
	if *a.VolumeId != "vol-1" || *a.InstanceId != "i-1" || *a.Device != "/dev/sdf" {
		t.Errorf("unexpected AttachVolume args: volume=%s instance=%s device=%s",
			*a.VolumeId, *a.InstanceId, *a.Device)
	}
}

func TestVolumeManager_SnapshotThenDelete(t *testing.T) {
	mock := &mockEC2Volumes{}
	mock.volumesByID = map[string]ec2types.Volume{
		"vol-archived": {VolumeId: strPtr("vol-archived"), State: ec2types.VolumeStateAvailable},
	}
	vm := NewEC2VolumeManagerWithClient(mock)
	vm.pollInterval = time.Millisecond
	vm.pollAttempts = 3

	snap, err := vm.SnapshotVolume(context.Background(), SnapshotOpts{
		VolumeID:    "vol-archived",
		ShardID:     "shard-archived",
		ParquetPath: "s3://weft-cold/shard-archived.parquet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap == "" {
		t.Fatal("expected snapshot id")
	}
	if len(mock.createSnapCalls) != 1 {
		t.Fatalf("expected 1 CreateSnapshot call, got %d", len(mock.createSnapCalls))
	}
	// Tags must include shard_id + parquet_path.
	tags := collectTags(mock.createSnapCalls[0].TagSpecifications)
	if tags["shard_id"] != "shard-archived" {
		t.Errorf("snapshot tag shard_id: got %q", tags["shard_id"])
	}
	if tags["parquet_path"] != "s3://weft-cold/shard-archived.parquet" {
		t.Errorf("snapshot tag parquet_path: got %q", tags["parquet_path"])
	}

	if err := vm.DeleteVolume(context.Background(), "vol-archived"); err != nil {
		t.Fatal(err)
	}
	if len(mock.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteVolume call, got %d", len(mock.deleteCalls))
	}
}

// ── helpers ─────────────────────────────────────────────────

func collectTags(specs []ec2types.TagSpecification) map[string]string {
	out := make(map[string]string)
	for _, ts := range specs {
		for _, t := range ts.Tags {
			if t.Key != nil && t.Value != nil {
				out[*t.Key] = *t.Value
			}
		}
	}
	return out
}
