package coordinator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/kevlarcon/weft/store"
)

// fakeVolumeManager records every call so tests can assert the
// sequence. Not the same as mockEC2Volumes — that one mocks the raw
// SDK client; this mocks the higher-level VolumeManager interface.
type fakeVolumeManager struct {
	mu sync.Mutex

	availableByTier map[string][]VolumeInfo  // keyed by tier+az ("small/us-east-2a")
	createdByShard  map[string]string        // shard_id → volume_id
	attached        []attachedPair
	tagged          map[string]string        // volume → shard id
	createErr       error
	attachErr       error

	nextVolID int
}

type attachedPair struct{ volumeID, instanceID, device string }

func (f *fakeVolumeManager) key(tier, az string) string { return tier + "/" + az }

func (f *fakeVolumeManager) CreateForShard(_ context.Context, opts CreateVolumeOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextVolID++
	id := fmt.Sprintf("vol-created-%d", f.nextVolID)
	if f.createdByShard == nil {
		f.createdByShard = make(map[string]string)
	}
	f.createdByShard[opts.ShardID] = id
	return id, nil
}

func (f *fakeVolumeManager) FindByShard(_ context.Context, shardID string) (*VolumeInfo, error) {
	return nil, nil
}

func (f *fakeVolumeManager) ListAvailableByTier(_ context.Context, tier, az string) ([]VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.availableByTier[f.key(tier, az)], nil
}

func (f *fakeVolumeManager) TagWithShardID(_ context.Context, volumeID, shardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tagged == nil {
		f.tagged = make(map[string]string)
	}
	f.tagged[volumeID] = shardID
	return nil
}

func (f *fakeVolumeManager) AttachToInstance(_ context.Context, volumeID, instanceID, device string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = append(f.attached, attachedPair{volumeID, instanceID, device})
	return f.attachErr
}

func (f *fakeVolumeManager) DetachFromInstance(_ context.Context, _ string, _ bool) error {
	return nil
}

func (f *fakeVolumeManager) DeleteVolume(_ context.Context, _ string) error { return nil }

func (f *fakeVolumeManager) SnapshotVolume(_ context.Context, _ SnapshotOpts) (string, error) {
	return "snap-stub", nil
}

// fakeEC2Instances returns a canned AZ for any DescribeInstances call.
type fakeEC2Instances struct {
	azByInstance map[string]string
}

func (f *fakeEC2Instances) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.InstanceIds) == 0 {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	id := in.InstanceIds[0]
	az := f.azByInstance[id]
	if az == "" {
		az = "us-east-2a"
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{
				InstanceId: &id,
				Placement:  &ec2types.Placement{AvailabilityZone: &az},
			}},
		}},
	}, nil
}

// fakeLifecycleASG records CompleteLifecycleAction calls so tests can
// assert the CONTINUE/ABANDON result.
type fakeLifecycleASG struct {
	mu    sync.Mutex
	calls []autoscaling.CompleteLifecycleActionInput
}

func (f *fakeLifecycleASG) CompleteLifecycleAction(_ context.Context, in *autoscaling.CompleteLifecycleActionInput, _ ...func(*autoscaling.Options)) (*autoscaling.CompleteLifecycleActionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, *in)
	return &autoscaling.CompleteLifecycleActionOutput{}, nil
}

func (f *fakeLifecycleASG) lastResult() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	r := f.calls[len(f.calls)-1].LifecycleActionResult
	if r == nil {
		return ""
	}
	return *r
}

// ── Tests ─────────────────────────────────────────

func baseHandler(vm VolumeManager, asg ASGLifecycleAPI, reg *ShardRegistry, storeIface HeartbeatStore) *LifecycleHandler {
	return handlerWithShardOps(vm, asg, reg, storeIface, nil)
}

func handlerWithShardOps(vm VolumeManager, asg ASGLifecycleAPI, reg *ShardRegistry, storeIface HeartbeatStore, shardOps ShardOpsStore) *LifecycleHandler {
	if storeIface == nil {
		// Tests that don't exercise recognisedAsDead can pass nil;
		// NewLifecycleHandler now refuses Registry-without-Store, so
		// supply an empty mock that returns no heartbeats.
		storeIface = &mockHeartbeatStore{}
	}
	return NewLifecycleHandler(LifecycleHandlerConfig{
		Volumes:      vm,
		EC2Instances: &fakeEC2Instances{},
		ASG:          asg,
		Store:        storeIface,
		ShardOps:     shardOps,
		Registry:     reg,
		TierByASG:    map[string]string{"weft-shard-small": "small"},
		SizeByTier:   map[string]int32{"small": 100},
		DeviceName:   "/dev/sdf",
		DeadFor:      2 * time.Minute,
	})
}

// seedLaunchingRow puts a LAUNCHING shard_records row into the given
// fake for a specific tier, matching what the reconciler would insert
// before calling the ASG. Tests that exercise the CHRISTEN branch need
// this so the hook's ClaimLaunchingForTier can find something to
// stamp.
func seedLaunchingRow(t *testing.T, fake *fakeRegistryStore, id, tier string) {
	t.Helper()
	if err := fake.SaveShard(&store.ShardRecord{
		ID:             id,
		State:          string(StateLaunching),
		Tier:           tier,
		BucketID:       defaultBucketID,
		Apps:           "null",
		WindowStart:    "2026-04-21T00:00:00Z",
		WindowEnd:      "2026-04-22T00:00:00Z",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		HeartbeatToken: "lifecycle-test-token",
	}); err != nil {
		t.Fatalf("seed LAUNCHING %s: %v", id, err)
	}
}

// No recovery candidates → christen: coordinator claims the
// reconciler's LAUNCHING row, creates a new volume tagged with its
// shard id, attaches, signals CONTINUE.
func TestLifecycle_NewFleet_Christens(t *testing.T) {
	vm := &fakeVolumeManager{}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	seedLaunchingRow(t, fake, "shard-fresh", "small")

	h := handlerWithShardOps(vm, asg, reg, nil, fake)

	ev := LifecycleEvent{
		ASGName:              "weft-shard-small",
		InstanceID:           "i-fresh",
		LifecycleHookName:    "shard-launch",
		LifecycleActionToken: "tok-1",
		LifecycleTransition:  "autoscaling:EC2_INSTANCE_LAUNCHING",
	}
	decision, err := h.HandleLaunchEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if decision != DecisionChristen {
		t.Errorf("decision: got %s want %s", decision, DecisionChristen)
	}
	if len(vm.attached) != 1 {
		t.Fatalf("expected 1 attach, got %d", len(vm.attached))
	}
	got := vm.attached[0]
	if got.instanceID != "i-fresh" || got.device != "/dev/sdf" {
		t.Errorf("unexpected attach: %+v", got)
	}
	if asg.lastResult() != "CONTINUE" {
		t.Errorf("CompleteLifecycleAction result: got %q want CONTINUE", asg.lastResult())
	}
	// The new volume's shard_id tag must match the claimed row, not
	// a placeholder — so the shard boots with its real identity in
	// place.
	if _, ok := vm.createdByShard["shard-fresh"]; !ok {
		t.Errorf("CreateForShard wasn't invoked with claimed row's id; createdByShard=%v", vm.createdByShard)
	}
}

// One orphan volume tagged with a shard_id we recognise, and no
// recent heartbeat → recover.
func TestLifecycle_OrphanVolume_Recovers(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-orphan", AZ: "us-east-2a", ShardID: "shard-old-01", State: "available", Tier: "small"},
			},
		},
	}
	asg := &fakeLifecycleASG{}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:    "shard-old-01",
		State: StateIngesting,
		Addr:  "10.0.0.1:9000",
	})

	// Heartbeat store reports nothing recent for shard-old-01 →
	// it's considered dead.
	storeFake := &mockHeartbeatStore{heartbeats: nil}

	h := baseHandler(vm, asg, reg, storeFake)

	ev := LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-replacement",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	}
	decision, err := h.HandleLaunchEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if decision != DecisionRecover {
		t.Errorf("decision: got %s want %s", decision, DecisionRecover)
	}
	if len(vm.attached) != 1 || vm.attached[0].volumeID != "vol-orphan" {
		t.Fatalf("expected attach of vol-orphan, got %+v", vm.attached)
	}
	if len(vm.createdByShard) != 0 {
		t.Errorf("expected no CreateVolume call on recovery, got %v", vm.createdByShard)
	}
	if asg.lastResult() != "CONTINUE" {
		t.Errorf("CompleteLifecycleAction result: got %q want CONTINUE", asg.lastResult())
	}
}

// Two orphan volumes for the same tier+AZ → recover the first one.
// The second volume will be picked up by the next instance launch.
func TestLifecycle_MultipleRecoverable_PicksFirst(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-a", AZ: "us-east-2a", ShardID: "shard-01", State: "available", Tier: "small"},
				{VolumeID: "vol-b", AZ: "us-east-2a", ShardID: "shard-02", State: "available", Tier: "small"},
			},
		},
	}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-01", State: StateIngesting})
	reg.Add(&ShardEntry{ID: "shard-02", State: StateIngesting})

	h := baseHandler(vm, asg, reg, &mockHeartbeatStore{heartbeats: nil})

	ev := LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-new",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	}
	decision, err := h.HandleLaunchEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != DecisionRecover {
		t.Errorf("decision: got %s want %s", decision, DecisionRecover)
	}
	if len(vm.attached) != 1 || vm.attached[0].volumeID != "vol-a" {
		t.Errorf("expected attach of vol-a, got %v", vm.attached)
	}
	if asg.lastResult() != "CONTINUE" {
		t.Errorf("CompleteLifecycleAction result: got %q want CONTINUE", asg.lastResult())
	}
}

// Models the 2026-05-04 production scenario: two SUSPENDED shards
// with orphan volumes in the same AZ/tier block a LAUNCHING shard
// from getting an instance. Before the fix, the handler abandoned
// with "ambiguous recovery"; after, it picks the first volume.
func TestLifecycle_SuspendedShardsWithOrphanVolumes_Recovers(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-07f6d57da29aa4599", AZ: "us-east-2a", ShardID: "shard-20260427T0000-334c5f87", State: "available", Tier: "small"},
				{VolumeID: "vol-0ec7d50ec17585a99", AZ: "us-east-2a", ShardID: "shard-20260504T0000-75aae944", State: "available", Tier: "small"},
			},
		},
	}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-20260427T0000-334c5f87", State: StateSuspended})
	reg.Add(&ShardEntry{ID: "shard-20260504T0000-75aae944", State: StateSuspended})
	reg.Add(&ShardEntry{ID: "shard-20260504T1800-ba56011d", State: StateLaunching})

	h := baseHandler(vm, asg, reg, &mockHeartbeatStore{heartbeats: nil})

	ev := LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-new-launch",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	}
	decision, err := h.HandleLaunchEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != DecisionRecover {
		t.Errorf("decision: got %s want %s", decision, DecisionRecover)
	}
	if len(vm.attached) != 1 || vm.attached[0].volumeID != "vol-07f6d57da29aa4599" {
		t.Errorf("expected attach of vol-07f6d57da29aa4599 (first recoverable), got %v", vm.attached)
	}
	if asg.lastResult() != "CONTINUE" {
		t.Errorf("CompleteLifecycleAction result: got %q want CONTINUE", asg.lastResult())
	}

	// The recovered shard's SUSPENDED entry must be cleared in-band.
	entry := reg.Get("shard-20260427T0000-334c5f87")
	if entry.State == StateSuspended {
		t.Errorf("recovered shard still SUSPENDED — should have been cleared in-band")
	}
	if entry.InstanceID != "i-new-launch" {
		t.Errorf("recovered shard InstanceID: got %q want %q", entry.InstanceID, "i-new-launch")
	}
	// The other SUSPENDED shard must still be SUSPENDED (not touched).
	other := reg.Get("shard-20260504T0000-75aae944")
	if other.State != StateSuspended {
		t.Errorf("unreovered shard: got %s want SUSPENDED", other.State)
	}
}

// An available volume whose shard_id is still heartbeating is NOT a
// recovery candidate — that shard isn't dead. With no recoverable
// volumes, the hook falls into the christen branch; a seeded
// LAUNCHING row for the tier lets it claim an identity for the new
// instance.
func TestLifecycle_LiveShardVolume_NotARecovery(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-live", AZ: "us-east-2a", ShardID: "shard-live", State: "available", Tier: "small"},
			},
		},
	}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-live", State: StateIngesting})

	storeFake := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-live", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	fake := newFakeRegistryStore()
	seedLaunchingRow(t, fake, "shard-new", "small")

	h := handlerWithShardOps(vm, asg, reg, storeFake, fake)

	ev := LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-new",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	}
	decision, err := h.HandleLaunchEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	// Neither a recover (shard isn't dead) nor would a second shard
	// trying to steal the live volume be safe — so this must be a
	// christen that creates a fresh volume for the new instance.
	if decision != DecisionChristen {
		t.Errorf("decision: got %s want %s", decision, DecisionChristen)
	}
}

// NewLifecycleHandler must refuse a Registry-without-Store config:
// recognisedAsDead consults live heartbeats from Store before letting
// the recovery scan yank a volume, and a nil Store can't answer. The
// 2026-05-03 prod outage was exactly this — cmd/coordinator/main.go
// passed ShardOps but forgot Store, recognisedAsDead's nil-store
// branch defaulted to "true (dead)", and the next launch event
// recovered the live shard's EBS volume out from under it. The fix
// is to refuse to construct the handler at all rather than to invent
// a runtime fallback for a misconfiguration.
func TestNewLifecycleHandler_PanicsWhenRegistrySetButStoreNil(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewLifecycleHandler with Registry but nil Store: expected panic, got none")
		}
	}()

	reg := NewShardRegistry()
	_ = NewLifecycleHandler(LifecycleHandlerConfig{
		Volumes:      &fakeVolumeManager{},
		EC2Instances: &fakeEC2Instances{},
		ASG:          &fakeLifecycleASG{},
		ShardOps:     newFakeRegistryStore(),
		Registry:     reg, // set
		// Store omitted — this is the bug we're catching.
		TierByASG:  map[string]string{"weft-shard-small": "small"},
		SizeByTier: map[string]int32{"small": 100},
	})
}

// SNS-wrapped payload should be unwrapped; raw payload should also
// parse.
func TestParseLifecycleEvent_SNSAndRaw(t *testing.T) {
	raw := []byte(`{"AutoScalingGroupName":"weft-shard-small","EC2InstanceId":"i-1","LifecycleTransition":"autoscaling:EC2_INSTANCE_LAUNCHING"}`)
	ev, err := ParseLifecycleEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ASGName != "weft-shard-small" || ev.InstanceID != "i-1" {
		t.Errorf("raw: unexpected event: %+v", ev)
	}

	sns := []byte(`{"Type":"Notification","Message":"{\"AutoScalingGroupName\":\"weft-shard-medium\",\"EC2InstanceId\":\"i-2\"}"}`)
	ev2, err := ParseLifecycleEvent(sns)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.ASGName != "weft-shard-medium" || ev2.InstanceID != "i-2" {
		t.Errorf("sns: unexpected event: %+v", ev2)
	}
}
