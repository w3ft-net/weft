package ec2

import (
	"context"
	"fmt"
	"github.com/w3ft-net/weft/logdb"
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
	createCalls    []*ec2.CreateVolumeInput
	createVolumeID string
	createErr      error

	// DescribeVolumes: keyed by first filter's value joined with ","
	describeErr    error
	volumesByShard map[string]ec2types.Volume // shard_id tag → volume
	volumesByID    map[string]ec2types.Volume // volume id → volume (for state polls)

	attachCalls []*ec2.AttachVolumeInput
	attachErr   error

	detachCalls []*ec2.DetachVolumeInput
	detachErr   error

	deleteCalls []*ec2.DeleteVolumeInput
	deleteErr   error

	createTagCalls []*ec2.CreateTagsInput
	createTagErr   error
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
	if len(in.VolumeIds) > 0 {
		var out []ec2types.Volume
		for _, id := range in.VolumeIds {
			if v, ok := m.volumesByID[id]; ok {
				out = append(out, v)
			}
		}
		return &ec2.DescribeVolumesOutput{Volumes: out}, nil
	}
	// General filter handling: apply all filters conjunctively across
	// volumesByID. Supported filter names: tag:<key>, availability-zone,
	// status.
	var out []ec2types.Volume
	for _, v := range m.volumesByID {
		if matchVolume(v, in.Filters) {
			out = append(out, v)
		}
	}
	// Back-compat with earlier tests that expected volumesByShard to
	// answer a direct tag:shard_id query even if the volume wasn't
	// registered in volumesByID.
	if len(out) == 0 {
		for _, f := range in.Filters {
			if f.Name != nil && *f.Name == "tag:shard_id" && len(f.Values) > 0 {
				if v, ok := m.volumesByShard[f.Values[0]]; ok {
					return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{v}}, nil
				}
			}
		}
	}
	return &ec2.DescribeVolumesOutput{Volumes: out}, nil
}

func matchVolume(v ec2types.Volume, filters []ec2types.Filter) bool {
	for _, f := range filters {
		if f.Name == nil {
			continue
		}
		name := *f.Name
		switch {
		case name == "availability-zone":
			if v.AvailabilityZone == nil || !contains(f.Values, *v.AvailabilityZone) {
				return false
			}
		case name == "status":
			if !contains(f.Values, string(v.State)) {
				return false
			}
		case len(name) > 4 && name[:4] == "tag:":
			key := name[4:]
			got := ""
			for _, t := range v.Tags {
				if t.Key != nil && *t.Key == key && t.Value != nil {
					got = *t.Value
				}
			}
			if !contains(f.Values, got) {
				return false
			}
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func (m *mockEC2Volumes) AttachVolume(_ context.Context, in *ec2.AttachVolumeInput, _ ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attachCalls = append(m.attachCalls, in)
	// Flip the attached volume's state so the post-attach
	// waitForVolumeState(InUse) poll succeeds — matches real AWS
	// behavior (a successful AttachVolume transitions the volume
	// through `attaching` → `in-use`).
	if m.attachErr == nil && in != nil && in.VolumeId != nil {
		if v, ok := m.volumesByID[*in.VolumeId]; ok {
			v.State = ec2types.VolumeStateInUse
			m.volumesByID[*in.VolumeId] = v
		}
	}
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

// ── Tests ───────────────────────────────────────────────────

func TestVolumeManager_CreateForShard_TagsAndSizesVolume(t *testing.T) {
	mock := &mockEC2Volumes{}
	vm := NewEC2VolumeManagerWithClient(mock)

	id, err := vm.CreateForShard(context.Background(), logdb.CreateVolumeOpts{
		ShardID: "shard-2026w16-small-01",
		Tier:    "small",
		AZ:      "us-east-2a",
		SizeGB:  100,
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

// Real AWS returns a just-created volume in `creating` state for a
// few seconds before it reaches `available`. AttachVolume against a
// `creating` volume fails with IncorrectState. This test pins the
// fix: AttachToInstance must wait for `available` before calling
// AttachVolume, otherwise we leak one volume per launch cycle in
// prod (seen 2026-04-17 during the EBS rollout).
func TestVolumeManager_AttachToInstance_WaitsForAvailable(t *testing.T) {
	mock := &mockEC2Volumes{
		volumesByID: map[string]ec2types.Volume{
			"vol-creating": {
				VolumeId: strPtr("vol-creating"),
				State:    ec2types.VolumeStateCreating, // starts not-available
			},
		},
	}
	vm := NewEC2VolumeManagerWithClient(mock)
	vm.pollInterval = 5 * time.Millisecond
	vm.pollAttempts = 100 // up to ~500ms — plenty for the flip below

	// Kick off the attach in a goroutine. It should block until we
	// flip the volume to `available`.
	done := make(chan error, 1)
	go func() {
		done <- vm.AttachToInstance(context.Background(), "vol-creating", "i-1", "/dev/sdf")
	}()

	// Give the poll loop a couple of ticks to observe `creating`,
	// then flip the state. A race-free flip is good enough here —
	// we're not testing exact timing, we're testing that the call
	// eventually waits and succeeds.
	time.Sleep(20 * time.Millisecond)
	if len(mock.attachCalls) != 0 {
		t.Fatalf("AttachVolume called while volume was still 'creating' — IncorrectState bug")
	}
	mock.mu.Lock()
	mock.volumesByID["vol-creating"] = ec2types.Volume{
		VolumeId: strPtr("vol-creating"),
		State:    ec2types.VolumeStateAvailable, // CreateVolume completed; attach can proceed
	}
	mock.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AttachToInstance: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("AttachToInstance did not return after volume became available")
	}

	if len(mock.attachCalls) != 1 {
		t.Fatalf("expected 1 AttachVolume call, got %d", len(mock.attachCalls))
	}
}

func TestVolumeManager_AttachToInstance(t *testing.T) {
	mock := &mockEC2Volumes{}
	// Volume must be `available` at the pre-attach poll, then
	// DescribeVolumes reports `in-use` after the attach. Both
	// happen on the same mock record; the pre-attach check is
	// what gates the call.
	mock.volumesByID = map[string]ec2types.Volume{
		"vol-1": {
			VolumeId: strPtr("vol-1"),
			State:    ec2types.VolumeStateAvailable,
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

func TestVolumeManager_DeleteVolume(t *testing.T) {
	mock := &mockEC2Volumes{}
	vm := NewEC2VolumeManagerWithClient(mock)

	if err := vm.DeleteVolume(context.Background(), "vol-archived"); err != nil {
		t.Fatal(err)
	}
	if len(mock.deleteCalls) != 1 || *mock.deleteCalls[0].VolumeId != "vol-archived" {
		t.Fatalf("expected 1 DeleteVolume call for vol-archived, got %v", mock.deleteCalls)
	}
}

// ListAvailableByTier returns volumes tagged with the requested tier
// that are in the `available` state (i.e. not currently attached) in
// the requested AZ. These are recovery candidates — their previous
// instance must have died and left the volume detached.
func TestVolumeManager_ListAvailableByTier(t *testing.T) {
	mock := &mockEC2Volumes{
		volumesByID: map[string]ec2types.Volume{
			"vol-avail-a": {
				VolumeId:         strPtr("vol-avail-a"),
				AvailabilityZone: strPtr("us-east-2a"),
				State:            ec2types.VolumeStateAvailable,
				Tags: []ec2types.Tag{
					{Key: strPtr("shard_tier"), Value: strPtr("small")},
					{Key: strPtr("shard_id"), Value: strPtr("shard-old-01")},
				},
			},
			"vol-inuse": {
				VolumeId:         strPtr("vol-inuse"),
				AvailabilityZone: strPtr("us-east-2a"),
				State:            ec2types.VolumeStateInUse,
				Tags: []ec2types.Tag{
					{Key: strPtr("shard_tier"), Value: strPtr("small")},
				},
			},
			"vol-other-az": {
				VolumeId:         strPtr("vol-other-az"),
				AvailabilityZone: strPtr("us-east-2b"),
				State:            ec2types.VolumeStateAvailable,
				Tags: []ec2types.Tag{
					{Key: strPtr("shard_tier"), Value: strPtr("small")},
				},
			},
			"vol-other-tier": {
				VolumeId:         strPtr("vol-other-tier"),
				AvailabilityZone: strPtr("us-east-2a"),
				State:            ec2types.VolumeStateAvailable,
				Tags: []ec2types.Tag{
					{Key: strPtr("shard_tier"), Value: strPtr("large")},
				},
			},
		},
	}
	vm := NewEC2VolumeManagerWithClient(mock)

	got, err := vm.ListAvailableByTier(context.Background(), "small", "us-east-2a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 available volume, got %d (%+v)", len(got), got)
	}
	if got[0].VolumeID != "vol-avail-a" {
		t.Errorf("VolumeID: got %q want vol-avail-a", got[0].VolumeID)
	}
}

// TagWithShardID stamps a volume that was created pre-christening with
// the shard id once we have one.
func TestVolumeManager_TagWithShardID(t *testing.T) {
	mock := &mockEC2Volumes{}
	vm := NewEC2VolumeManagerWithClient(mock)

	if err := vm.TagWithShardID(context.Background(), "vol-new", "shard-2026w16-small-01"); err != nil {
		t.Fatal(err)
	}

	if len(mock.createTagCalls) != 1 {
		t.Fatalf("expected 1 CreateTags call, got %d", len(mock.createTagCalls))
	}
	call := mock.createTagCalls[0]
	if len(call.Resources) != 1 || call.Resources[0] != "vol-new" {
		t.Errorf("resources: got %v want [vol-new]", call.Resources)
	}
	got := map[string]string{}
	for _, tag := range call.Tags {
		if tag.Key != nil && tag.Value != nil {
			got[*tag.Key] = *tag.Value
		}
	}
	if got["shard_id"] != "shard-2026w16-small-01" {
		t.Errorf("tag shard_id: got %q", got["shard_id"])
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

// An upgrade's swap and rollback read where the volume is attached to
// decide their next step, so FindByShard must report it.
func TestVolumeManager_FindByShard_ReportsTheAttachedInstance(t *testing.T) {
	mock := &mockEC2Volumes{
		volumesByShard: map[string]ec2types.Volume{
			"shard-x": {
				VolumeId: strPtr("vol-123"),
				State:    ec2types.VolumeStateInUse,
				Attachments: []ec2types.VolumeAttachment{
					{InstanceId: strPtr("i-gone"), State: ec2types.VolumeAttachmentStateDetached},
					{InstanceId: strPtr("i-now"), State: ec2types.VolumeAttachmentStateAttached},
				},
			},
		},
	}
	got, err := NewEC2VolumeManagerWithClient(mock).FindByShard(context.Background(), "shard-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != "i-now" {
		t.Fatalf("InstanceID = %q, want the attached instance i-now", got.InstanceID)
	}
}
