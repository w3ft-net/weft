//go:build test

package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ── fakeEC2: a minimal EC2RunAPI ────────────────────────────────────
//
// RunInstances synthesises one instance (tags from TagSpecifications, state
// pending→running after launchDelay describe-by-id polls). DescribeInstances
// serves both the by-id wait-for-running poll (which advances state) and the
// by-tag idempotency lookup (tag:shard_id + instance-state-name filter).
// TerminateInstances marks an instance terminated, erroring on an unknown id.

type fakeEC2Instance struct {
	id           string
	tags         map[string]string
	imageID      string // ImageId from the RunInstances request ("" if none)
	state        string // pending | running | terminated
	pendingPolls int
	launched     time.Time // zero unless a test sets it (the export sweep reads LaunchTime)
}

type fakeEC2 struct {
	mu          sync.Mutex
	insts       []*fakeEC2Instance
	nextID      int
	launchDelay int // describe-by-id polls an instance spends pending
	runErr      error
	terminated  []string
}

func newFakeEC2() *fakeEC2 { return &fakeEC2{} }

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return nil, f.runErr
	}
	f.nextID++
	tags := map[string]string{}
	for _, ts := range in.TagSpecifications {
		for _, t := range ts.Tags {
			if t.Key != nil && t.Value != nil {
				tags[*t.Key] = *t.Value
			}
		}
	}
	inst := &fakeEC2Instance{
		id:           fmt.Sprintf("i-%d", f.nextID),
		tags:         tags,
		imageID:      aws.ToString(in.ImageId),
		state:        "pending",
		pendingPolls: f.launchDelay,
	}
	if inst.pendingPolls == 0 {
		inst.state = "running"
	}
	f.insts = append(f.insts, inst)
	return &ec2.RunInstancesOutput{
		Instances: []ec2types.Instance{{InstanceId: aws.String(inst.id)}},
	}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.InstanceIds {
		found := false
		for _, inst := range f.insts {
			if inst.id == id {
				inst.state = "terminated"
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("InvalidInstanceID.NotFound: the instance ID %q does not exist", id)
		}
		f.terminated = append(f.terminated, id)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var match []*fakeEC2Instance
	if len(in.InstanceIds) > 0 {
		ids := map[string]bool{}
		for _, id := range in.InstanceIds {
			ids[id] = true
		}
		for _, inst := range f.insts {
			if !ids[inst.id] {
				continue
			}
			if inst.state == "pending" { // advance toward running on each poll
				if inst.pendingPolls > 0 {
					inst.pendingPolls--
				}
				if inst.pendingPolls == 0 {
					inst.state = "running"
				}
			}
			match = append(match, inst)
		}
	} else {
		wantShard := ec2FilterValue(in.Filters, "tag:shard_id")
		wantKey := ec2FilterValue(in.Filters, "tag-key")
		wantStates := ec2FilterValues(in.Filters, "instance-state-name")
		for _, inst := range f.insts {
			if wantShard != "" && inst.tags["shard_id"] != wantShard {
				continue
			}
			if _, has := inst.tags[wantKey]; wantKey != "" && !has {
				continue
			}
			if len(wantStates) > 0 && !ec2Contains(wantStates, inst.state) {
				continue
			}
			match = append(match, inst)
		}
	}
	var out []ec2types.Instance
	for _, m := range match {
		inst := ec2types.Instance{
			InstanceId: aws.String(m.id),
			State:      &ec2types.InstanceState{Name: ec2types.InstanceStateName(m.state)},
			ImageId:    aws.String(m.imageID),
		}
		if !m.launched.IsZero() {
			inst.LaunchTime = aws.Time(m.launched)
		}
		for k, v := range m.tags {
			inst.Tags = append(inst.Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		out = append(out, inst)
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: out}},
	}, nil
}

func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.Resources {
		inst := f.find(id)
		if inst == nil {
			return nil, fmt.Errorf("InvalidInstanceID.NotFound: %q", id)
		}
		for _, t := range in.Tags {
			inst.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) DeleteTags(_ context.Context, in *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.Resources {
		inst := f.find(id)
		if inst == nil {
			return nil, fmt.Errorf("InvalidInstanceID.NotFound: %q", id)
		}
		for _, t := range in.Tags {
			delete(inst.tags, aws.ToString(t.Key))
		}
	}
	return &ec2.DeleteTagsOutput{}, nil
}

// find returns the instance with id, or nil. Caller holds f.mu.
func (f *fakeEC2) find(id string) *fakeEC2Instance {
	for _, inst := range f.insts {
		if inst.id == id {
			return inst
		}
	}
	return nil
}

func ec2FilterValue(fs []ec2types.Filter, name string) string {
	if v := ec2FilterValues(fs, name); len(v) > 0 {
		return v[0]
	}
	return ""
}

func ec2FilterValues(fs []ec2types.Filter, name string) []string {
	for _, f := range fs {
		if f.Name != nil && *f.Name == name {
			return f.Values
		}
	}
	return nil
}

func ec2Contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func newTestEC2Provisioner(client EC2RunAPI, vm VolumeManager) *EC2Provisioner {
	p := NewEC2ProvisionerWithClient(client, vm,
		map[string]map[string]string{"small": {"us-east-2a": "subnet-a", "us-east-2b": "subnet-b"}},
		map[string]string{"small": "lt-small"},
		"", // no AMI override: exercise the launch-template fallback path
	)
	p.pollInterval = time.Millisecond
	return p
}

func ec2InstanceCount(f *fakeEC2) (live int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.insts {
		if i.state != "terminated" {
			live++
		}
	}
	return live
}

// ── tests ───────────────────────────────────────────────────────────

func TestEC2Provisioner_LaunchFresh(t *testing.T) {
	f := newFakeEC2()
	vm := &fakeVolumeManager{}
	p := newTestEC2Provisioner(f, vm)

	res, err := p.Launch(context.Background(), LaunchSpec{
		ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID != "i-1" || res.AZ != "us-east-2a" || res.VolumeID != "vol-created-1" {
		t.Fatalf("result: got %+v", res)
	}
	// Tagged shard_id at birth.
	if f.insts[0].tags["shard_id"] != "s-1" {
		t.Fatalf("instance not tagged shard_id=s-1: %v", f.insts[0].tags)
	}
	// Volume created and attached at /dev/sdf.
	if len(vm.attached) != 1 || vm.attached[0] != (attachedPair{"vol-created-1", "i-1", shardVolumeDevice}) {
		t.Fatalf("attach: got %v", vm.attached)
	}
	if got := ec2InstanceCount(f); got != 1 {
		t.Fatalf("want 1 live instance, got %d", got)
	}
	// No AMI override configured → RunInstances carries no ImageId and the
	// launch template's baked image stands (the off-EC2/IMDS-failure fallback).
	if f.insts[0].imageID != "" {
		t.Fatalf("expected no ImageId override, got %q", f.insts[0].imageID)
	}
}

// TestEC2Provisioner_LaunchOverridesAMI is the dead-launch-template guard: when
// the provisioner knows the coordinator's own AMI, every shard launch overrides
// the template's baked image with it, so a stale/deleted template AMI can't wedge
// the fleet.
func TestEC2Provisioner_LaunchOverridesAMI(t *testing.T) {
	f := newFakeEC2()
	vm := &fakeVolumeManager{}
	p := NewEC2ProvisionerWithClient(f, vm,
		map[string]map[string]string{"small": {"us-east-2a": "subnet-a"}},
		map[string]string{"small": "lt-small"},
		"ami-own123", // coordinator's own (live) image
	)
	p.pollInterval = time.Millisecond

	if _, err := p.Launch(context.Background(), LaunchSpec{
		ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.insts[0].imageID != "ami-own123" {
		t.Fatalf("RunInstances ImageId: want ami-own123, got %q", f.insts[0].imageID)
	}
}

// TestEC2Provisioner_IdempotentReplay is the ghost-proof property: a
// crash-replay of a fresh launch (instance already running + tagged, volume
// already created) adopts the in-flight instance and reattaches the existing
// volume instead of minting a twin. With no second launcher in the picture,
// this is the whole story — there is no path to an unowned instance.
func TestEC2Provisioner_IdempotentReplay(t *testing.T) {
	f := newFakeEC2()
	vm := &fakeVolumeManager{}
	p := newTestEC2Provisioner(f, vm)
	ctx := context.Background()

	first, err := p.Launch(ctx, LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20})
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	// Simulate the crash-replay state: the instance is up and tagged, and the
	// volume it created is now discoverable by tag (the persist that would
	// have recorded it never happened — that's the crash).
	vm.findResult = map[string]*VolumeInfo{"s-1": {VolumeID: first.VolumeID}}

	replay, err := p.Launch(ctx, LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20})
	if err != nil {
		t.Fatalf("replay launch: %v", err)
	}
	if replay.InstanceID != first.InstanceID {
		t.Fatalf("replay minted a twin: first=%s replay=%s", first.InstanceID, replay.InstanceID)
	}
	if replay.VolumeID != first.VolumeID {
		t.Fatalf("replay made a second volume: first=%s replay=%s", first.VolumeID, replay.VolumeID)
	}
	if got := ec2InstanceCount(f); got != 1 {
		t.Fatalf("want 1 live instance after replay, got %d", got)
	}
	if vm.nextVolID != 1 {
		t.Fatalf("want exactly 1 volume created, got %d", vm.nextVolID)
	}
}

// TestEC2Provisioner_RecoveryReattach: a recovery relaunch (old instance
// already reaped, ExistingVolumeID set) launches a fresh instance and
// reattaches the surviving volume — no new volume, distinct instance.
func TestEC2Provisioner_RecoveryReattach(t *testing.T) {
	f := newFakeEC2()
	vm := &fakeVolumeManager{}
	p := newTestEC2Provisioner(f, vm)
	ctx := context.Background()

	first, err := p.Launch(ctx, LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20})
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	// Reap it (the recovery reap), then relaunch with the surviving volume.
	if err := p.Terminate(ctx, first.InstanceID); err != nil {
		t.Fatalf("reap: %v", err)
	}
	rec, err := p.Launch(ctx, LaunchSpec{
		ShardID: "s-1", Tier: "small", AZ: "us-east-2a", ExistingVolumeID: first.VolumeID,
	})
	if err != nil {
		t.Fatalf("recovery launch: %v", err)
	}
	if rec.InstanceID == first.InstanceID {
		t.Fatalf("recovery adopted the reaped instance %s", rec.InstanceID)
	}
	if rec.VolumeID != first.VolumeID {
		t.Fatalf("recovery should reattach %s, got %s", first.VolumeID, rec.VolumeID)
	}
	if vm.nextVolID != 1 {
		t.Fatalf("recovery created a new volume; want 1 total, got %d", vm.nextVolID)
	}
	// reattached to the new instance
	last := vm.attached[len(vm.attached)-1]
	if last != (attachedPair{first.VolumeID, rec.InstanceID, shardVolumeDevice}) {
		t.Fatalf("reattach: got %v", last)
	}
}

func TestEC2Provisioner_TerminateAlreadyGoneIsSuccess(t *testing.T) {
	f := newFakeEC2()
	p := newTestEC2Provisioner(f, &fakeVolumeManager{})
	if err := p.Terminate(context.Background(), "i-does-not-exist"); err != nil {
		t.Fatalf("terminate of absent instance should be success, got %v", err)
	}
}

func TestEC2Provisioner_WaitsForRunning(t *testing.T) {
	f := newFakeEC2()
	f.launchDelay = 3 // pending for 3 describe polls before running
	vm := &fakeVolumeManager{}
	p := newTestEC2Provisioner(f, vm)

	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// Volume attached only after the instance reached running.
	if len(vm.attached) != 1 || vm.attached[0].instanceID != res.InstanceID {
		t.Fatalf("attach should happen after running: %v", vm.attached)
	}
	if f.insts[0].state != "running" {
		t.Fatalf("instance not running: %s", f.insts[0].state)
	}
}

func TestEC2Provisioner_UnknownAZorTierErrorsFast(t *testing.T) {
	p := newTestEC2Provisioner(newFakeEC2(), &fakeVolumeManager{})
	if _, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-west-1a"}); err == nil {
		t.Fatal("want error for unconfigured AZ")
	}
	if _, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-1", Tier: "large", AZ: "us-east-2a"}); err == nil {
		t.Fatal("want error for tier with no subnet/template")
	}
}

func TestEC2Provisioner_StripeAZ(t *testing.T) {
	p := newTestEC2Provisioner(newFakeEC2(), &fakeVolumeManager{})
	// AZs sorted: us-east-2a (0), us-east-2b (1).
	if got := p.StripeAZ("small", 0); got != "us-east-2a" {
		t.Fatalf("stripe 0: got %q", got)
	}
	if got := p.StripeAZ("small", 1); got != "us-east-2b" {
		t.Fatalf("stripe 1: got %q", got)
	}
	if got := p.StripeAZ("small", 2); got != "us-east-2a" {
		t.Fatalf("stripe 2 (wraps): got %q", got)
	}
	if got := p.StripeAZ("nope", 0); got != "" {
		t.Fatalf("unknown tier: got %q", got)
	}
}
