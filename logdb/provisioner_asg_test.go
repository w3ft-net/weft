package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// ── fakes ───────────────────────────────────────────────────────

// fakeASGInstance models one instance in a fake ASG, with an optional
// countdown of Describe polls it spends Pending before going InService.
type fakeASGInstance struct {
	id           string
	pendingPolls int
}

type fakeASGGroup struct {
	desired      int32
	instances    []*fakeASGInstance
	launchDelay  int // pendingPolls stamped on instances this group launches
	nextInstance int
}

// fakeASG is a minimal ASGApi: SetDesiredCapacity synthesises new
// instances to match the requested capacity; Describe reports them,
// flipping Pending→InService as each instance's poll countdown elapses.
type fakeASG struct {
	mu         sync.Mutex
	groups     map[string]*fakeASGGroup
	setErr     error
	terminated []string
	termErr    error // returned when terminating an unknown instance
}

func newFakeASG(names ...string) *fakeASG {
	f := &fakeASG{groups: map[string]*fakeASGGroup{}}
	for _, n := range names {
		f.groups[n] = &fakeASGGroup{}
	}
	return f
}

func (f *fakeASG) SetDesiredCapacity(_ context.Context, in *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return nil, f.setErr
	}
	g := f.groups[*in.AutoScalingGroupName]
	g.desired = *in.DesiredCapacity
	for int32(len(g.instances)) < g.desired {
		g.nextInstance++
		g.instances = append(g.instances, &fakeASGInstance{
			id:           fmt.Sprintf("i-%s-%d", *in.AutoScalingGroupName, g.nextInstance),
			pendingPolls: g.launchDelay,
		})
	}
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (f *fakeASG) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.AutoScalingGroupNames[0]
	g, ok := f.groups[name]
	if !ok {
		return &autoscaling.DescribeAutoScalingGroupsOutput{}, nil
	}
	var insts []astypes.Instance
	for _, fi := range g.instances {
		state := astypes.LifecycleStateInService
		if fi.pendingPolls > 0 {
			state = astypes.LifecycleStatePending
			fi.pendingPolls--
		}
		insts = append(insts, astypes.Instance{
			InstanceId:     strPtr(fi.id),
			LifecycleState: state,
		})
	}
	desired := g.desired
	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []astypes.AutoScalingGroup{{
			AutoScalingGroupName: strPtr(name),
			DesiredCapacity:      &desired,
			Instances:            insts,
		}},
	}, nil
}

func (f *fakeASG) TerminateInstanceInAutoScalingGroup(_ context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, *in.InstanceId)
	for _, g := range f.groups {
		for i, fi := range g.instances {
			if fi.id == *in.InstanceId {
				g.instances = append(g.instances[:i], g.instances[i+1:]...)
				if !*in.ShouldDecrementDesiredCapacity {
					return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
				}
				g.desired--
				return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
			}
		}
	}
	return nil, f.termErr // unknown instance
}

// fakeTagger records CreateTags calls.
type fakeTagger struct {
	mu     sync.Mutex
	tagged map[string]map[string]string // instance → tag key → value
	err    error
}

func newFakeTagger() *fakeTagger { return &fakeTagger{tagged: map[string]map[string]string{}} }

func (f *fakeTagger) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for _, res := range in.Resources {
		m := f.tagged[res]
		if m == nil {
			m = map[string]string{}
			f.tagged[res] = m
		}
		for _, t := range in.Tags {
			m[*t.Key] = *t.Value
		}
	}
	return &ec2.CreateTagsOutput{}, nil
}

// twoAZSmall is the ASG map used across most tests: one tier, two AZs.
func twoAZSmall() map[string]map[string]string {
	return map[string]map[string]string{
		"small": {
			"us-east-2a": "asg-small-a",
			"us-east-2b": "asg-small-b",
		},
	}
}

func newTestProvisioner(t *testing.T, asg *fakeASG, tag *fakeTagger, vm *fakeVolumeManager, asgs map[string]map[string]string) *ASGProvisioner {
	t.Helper()
	p := NewASGProvisionerWithClients(asg, tag, vm, asgs)
	p.pollInterval = time.Millisecond // keep the poll loop fast in tests
	return p
}

// ── tests ───────────────────────────────────────────────────────

func TestASGProvisioner_LaunchFresh(t *testing.T) {
	asg := newFakeASG("asg-small-a", "asg-small-b")
	tag := newFakeTagger()
	vm := &fakeVolumeManager{}
	p := newTestProvisioner(t, asg, tag, vm, twoAZSmall())

	res, err := p.Launch(context.Background(), LaunchSpec{
		ShardID: "s-1", Tier: "small", AZ: "us-east-2a", SizeGB: 20,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID != "i-asg-small-a-1" {
		t.Fatalf("InstanceID: got %q", res.InstanceID)
	}
	if res.AZ != "us-east-2a" {
		t.Fatalf("AZ: got %q", res.AZ)
	}
	// instance tagged with the shard id
	if tag.tagged[res.InstanceID]["shard_id"] != "s-1" {
		t.Fatalf("instance not tagged shard_id=s-1: %v", tag.tagged[res.InstanceID])
	}
	// fresh launch creates a volume and attaches it to the instance
	if vm.createdByShard["s-1"] == "" {
		t.Fatalf("no volume created for s-1")
	}
	if res.VolumeID != vm.createdByShard["s-1"] {
		t.Fatalf("result volume %q != created %q", res.VolumeID, vm.createdByShard["s-1"])
	}
	assertAttached(t, vm, res.VolumeID, res.InstanceID)
	// desired bumped 0 → 1
	if asg.groups["asg-small-a"].desired != 1 {
		t.Fatalf("desired: got %d want 1", asg.groups["asg-small-a"].desired)
	}
}

func TestASGProvisioner_LaunchRecovery_ReattachesExistingVolume(t *testing.T) {
	asg := newFakeASG("asg-small-a", "asg-small-b")
	tag := newFakeTagger()
	vm := &fakeVolumeManager{}
	p := newTestProvisioner(t, asg, tag, vm, twoAZSmall())

	res, err := p.Launch(context.Background(), LaunchSpec{
		ShardID: "s-1", Tier: "small", AZ: "us-east-2a", ExistingVolumeID: "vol-survivor",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// recovery must NOT create a fresh volume...
	if len(vm.createdByShard) != 0 {
		t.Fatalf("recovery created a volume: %v", vm.createdByShard)
	}
	// ...and must reattach the surviving one.
	if res.VolumeID != "vol-survivor" {
		t.Fatalf("VolumeID: got %q want vol-survivor", res.VolumeID)
	}
	assertAttached(t, vm, "vol-survivor", res.InstanceID)
}

func TestASGProvisioner_LaunchUnknownAZ_ErrorsFast(t *testing.T) {
	asg := newFakeASG("asg-small-a", "asg-small-b")
	p := newTestProvisioner(t, asg, newFakeTagger(), &fakeVolumeManager{}, twoAZSmall())

	_, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2c"})
	if err == nil {
		t.Fatal("expected error for unconfigured AZ")
	}
	// must not have touched the ASG at all
	if asg.groups["asg-small-a"].desired != 0 {
		t.Fatalf("desired changed on a rejected launch: %d", asg.groups["asg-small-a"].desired)
	}
}

func TestASGProvisioner_LaunchWaitsForInService(t *testing.T) {
	asg := newFakeASG("asg-small-a")
	asg.groups["asg-small-a"].launchDelay = 3 // Pending for 3 polls, then InService
	p := newTestProvisioner(t, asg, newFakeTagger(), &fakeVolumeManager{},
		map[string]map[string]string{"small": {"us-east-2a": "asg-small-a"}})

	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2a"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID == "" {
		t.Fatal("expected an instance id once InService")
	}
}

func TestASGProvisioner_LaunchTimesOutIfNeverInService(t *testing.T) {
	asg := newFakeASG("asg-small-a")
	asg.groups["asg-small-a"].launchDelay = 1000 // never reaches InService within budget
	p := newTestProvisioner(t, asg, newFakeTagger(), &fakeVolumeManager{},
		map[string]map[string]string{"small": {"us-east-2a": "asg-small-a"}})
	p.pollAttempts = 3

	_, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-1", Tier: "small", AZ: "us-east-2a"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestASGProvisioner_StripeAZ(t *testing.T) {
	p := newTestProvisioner(t, newFakeASG(), newFakeTagger(), &fakeVolumeManager{}, twoAZSmall())

	// AZsForTier is sorted, so striping is deterministic.
	if got := p.AZsForTier("small"); len(got) != 2 || got[0] != "us-east-2a" || got[1] != "us-east-2b" {
		t.Fatalf("AZsForTier: %v", got)
	}
	want := []string{"us-east-2a", "us-east-2b", "us-east-2a", "us-east-2b"}
	for i, w := range want {
		if got := p.StripeAZ("small", i); got != w {
			t.Fatalf("StripeAZ(small,%d): got %q want %q", i, got, w)
		}
	}
	if got := p.StripeAZ("nonexistent", 0); got != "" {
		t.Fatalf("StripeAZ on unknown tier: got %q want \"\"", got)
	}
}

func TestASGProvisioner_TerminateDecrements(t *testing.T) {
	asg := newFakeASG("asg-small-a")
	asg.groups["asg-small-a"].desired = 1
	asg.groups["asg-small-a"].instances = []*fakeASGInstance{{id: "i-live"}}
	p := newTestProvisioner(t, asg, newFakeTagger(), &fakeVolumeManager{},
		map[string]map[string]string{"small": {"us-east-2a": "asg-small-a"}})

	if err := p.Terminate(context.Background(), "i-live"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if asg.groups["asg-small-a"].desired != 0 {
		t.Fatalf("Terminate didn't decrement: %d", asg.groups["asg-small-a"].desired)
	}
}

func TestASGProvisioner_TerminateAlreadyGoneIsSuccess(t *testing.T) {
	asg := newFakeASG("asg-small-a")
	asg.termErr = fmt.Errorf("instance i-ghost not found")
	p := newTestProvisioner(t, asg, newFakeTagger(), &fakeVolumeManager{},
		map[string]map[string]string{"small": {"us-east-2a": "asg-small-a"}})

	if err := p.Terminate(context.Background(), "i-ghost"); err != nil {
		t.Fatalf("already-gone instance should be success, got %v", err)
	}
}

func TestASGProvisioner_ConcurrentLaunchesIntoSameASGSerialize(t *testing.T) {
	asg := newFakeASG("asg-small-a")
	p := newTestProvisioner(t, asg, newFakeTagger(), &fakeVolumeManager{},
		map[string]map[string]string{"small": {"us-east-2a": "asg-small-a"}})

	var wg sync.WaitGroup
	ids := make([]string, 2)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := p.Launch(context.Background(), LaunchSpec{
				ShardID: fmt.Sprintf("s-%d", i), Tier: "small", AZ: "us-east-2a",
			})
			if err != nil {
				t.Errorf("Launch %d: %v", i, err)
				return
			}
			ids[i] = res.InstanceID
		}(i)
	}
	wg.Wait()

	// Two serialized launches must yield two distinct instances and
	// desired=2 — not one shared instance from a lost +1 race.
	if ids[0] == ids[1] {
		t.Fatalf("both launches returned the same instance %q — lock didn't serialize the read-modify-write", ids[0])
	}
	if asg.groups["asg-small-a"].desired != 2 {
		t.Fatalf("desired: got %d want 2", asg.groups["asg-small-a"].desired)
	}
}

func assertAttached(t *testing.T, vm *fakeVolumeManager, volumeID, instanceID string) {
	t.Helper()
	for _, a := range vm.attached {
		if a.volumeID == volumeID && a.instanceID == instanceID {
			return
		}
	}
	t.Fatalf("volume %q not attached to instance %q; attached=%v", volumeID, instanceID, vm.attached)
}
