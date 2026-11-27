package ec2

import (
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/logdb"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ── fakeExportEC2: tag-filtering EC2ExportAPI ───────────────────────

type fakeSnapshot struct {
	id      string
	tags    map[string]string
	state   ec2types.SnapshotState
	started time.Time // zero unless a test sets it (the export sweep reads StartTime)
}

type fakeExportEC2 struct {
	mu        sync.Mutex
	snaps     []*fakeSnapshot
	insts     []*fakeEC2Instance
	runInputs []*ec2.RunInstancesInput
	nextID    int

	createSnapshotCalls int
	deletedSnapshots    []string
	terminated          []string
}

func tagMap(specs []ec2types.TagSpecification, rt ec2types.ResourceType) map[string]string {
	out := map[string]string{}
	for _, ts := range specs {
		if ts.ResourceType != rt {
			continue
		}
		for _, t := range ts.Tags {
			out[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return out
}

func matchesTagFilters(tags map[string]string, filters []ec2types.Filter) bool {
	for _, f := range filters {
		name := aws.ToString(f.Name)
		if name == "tag-key" { // has the tag at all, any value
			for _, k := range f.Values {
				if _, has := tags[k]; !has {
					return false
				}
			}
			continue
		}
		key, ok := strings.CutPrefix(name, "tag:")
		if !ok {
			continue
		}
		if !ec2Contains(f.Values, tags[key]) {
			return false
		}
	}
	return true
}

func tagList(tags map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for k, v := range tags {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func (f *fakeExportEC2) CreateSnapshot(_ context.Context, in *ec2.CreateSnapshotInput, _ ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createSnapshotCalls++
	f.nextID++
	s := &fakeSnapshot{
		id:    fmt.Sprintf("snap-%d", f.nextID),
		tags:  tagMap(in.TagSpecifications, ec2types.ResourceTypeSnapshot),
		state: ec2types.SnapshotStatePending,
	}
	f.snaps = append(f.snaps, s)
	return &ec2.CreateSnapshotOutput{SnapshotId: aws.String(s.id), State: s.state}, nil
}

func (f *fakeExportEC2) DescribeSnapshots(_ context.Context, in *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ec2types.Snapshot
	for _, s := range f.snaps {
		if len(in.SnapshotIds) > 0 && !ec2Contains(in.SnapshotIds, s.id) {
			continue
		}
		if !matchesTagFilters(s.tags, in.Filters) {
			continue
		}
		snap := ec2types.Snapshot{
			SnapshotId: aws.String(s.id), State: s.state, Progress: aws.String("42%"), Tags: tagList(s.tags),
		}
		if !s.started.IsZero() {
			snap.StartTime = aws.Time(s.started)
		}
		out = append(out, snap)
	}
	return &ec2.DescribeSnapshotsOutput{Snapshots: out}, nil
}

func (f *fakeExportEC2) DeleteSnapshot(_ context.Context, in *ec2.DeleteSnapshotInput, _ ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.SnapshotId)
	for i, s := range f.snaps {
		if s.id == id {
			f.snaps = append(f.snaps[:i], f.snaps[i+1:]...)
			f.deletedSnapshots = append(f.deletedSnapshots, id)
			return &ec2.DeleteSnapshotOutput{}, nil
		}
	}
	return nil, fmt.Errorf("InvalidSnapshot.NotFound: the snapshot %q does not exist", id)
}

func (f *fakeExportEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runInputs = append(f.runInputs, in)
	f.nextID++
	inst := &fakeEC2Instance{
		id:    fmt.Sprintf("i-exp-%d", f.nextID),
		tags:  tagMap(in.TagSpecifications, ec2types.ResourceTypeInstance),
		state: "pending",
	}
	f.insts = append(f.insts, inst)
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String(inst.id)}}}, nil
}

func (f *fakeExportEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wantStates := ec2FilterValues(in.Filters, "instance-state-name")
	var out []ec2types.Instance
	for _, inst := range f.insts {
		if len(in.InstanceIds) > 0 && !ec2Contains(in.InstanceIds, inst.id) {
			continue
		}
		if !matchesTagFilters(inst.tags, in.Filters) {
			continue
		}
		if len(wantStates) > 0 && !ec2Contains(wantStates, inst.state) {
			continue
		}
		i := ec2types.Instance{
			InstanceId: aws.String(inst.id),
			State:      &ec2types.InstanceState{Name: ec2types.InstanceStateName(inst.state)},
			Tags:       tagList(inst.tags),
		}
		if !inst.launched.IsZero() {
			i.LaunchTime = aws.Time(inst.launched)
		}
		out = append(out, i)
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (f *fakeExportEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
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

func newTestExportResources(api EC2ExportAPI) *ExportResources {
	return NewExportResourcesWithClient(api, map[string]string{"us-east-2a": "subnet-a", "us-east-2b": "subnet-b"}, "lt-export", "ami-coord")
}

// ── snapshots ───────────────────────────────────────────────────────

func TestExportResources_CreateSnapshotTagsAndIsIdempotent(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)

	id1, err := x.CreateSnapshot(context.Background(), logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "vol-9"})
	if err != nil {
		t.Fatal(err)
	}
	if got := api.snaps[0].tags; got["export_shard_id"] != "s-1" || got["export_attempt"] != "1" {
		t.Fatalf("snapshot tags = %v", got)
	}
	if _, has := api.snaps[0].tags["shard_id"]; has {
		t.Fatalf("snapshot carries the shard_id tag, which shard lookups key on: %v", api.snaps[0].tags)
	}

	// A crash-replay of the same attempt adopts the snapshot rather than
	// minting a second one.
	id2, err := x.CreateSnapshot(context.Background(), logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "vol-9"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 || api.createSnapshotCalls != 1 {
		t.Fatalf("replay minted a second snapshot: %q vs %q, calls=%d", id1, id2, api.createSnapshotCalls)
	}

	// A new attempt gets its own snapshot.
	id3, err := x.CreateSnapshot(context.Background(), logdb.SnapshotSpec{ShardID: "s-1", Attempt: 2, VolumeID: "vol-9"})
	if err != nil {
		t.Fatal(err)
	}
	if id3 == id1 {
		t.Fatalf("attempt 2 reused attempt 1's snapshot %q", id3)
	}
}

func TestExportResources_CreateSnapshotDoesNotAdoptAnErroredSnapshot(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	id1, _ := x.CreateSnapshot(context.Background(), logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "vol-9"})
	api.snaps[0].state = ec2types.SnapshotStateError

	id2, err := x.CreateSnapshot(context.Background(), logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "vol-9"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id1 {
		t.Fatal("adopted a snapshot that is in the error state")
	}
}

func TestExportResources_CreateSnapshotValidates(t *testing.T) {
	x := newTestExportResources(&fakeExportEC2{})
	for _, spec := range []logdb.SnapshotSpec{
		{Attempt: 1, VolumeID: "v"}, {ShardID: "s", VolumeID: "v"}, {ShardID: "s", Attempt: 1},
	} {
		if _, err := x.CreateSnapshot(context.Background(), spec); err == nil {
			t.Errorf("CreateSnapshot(%+v) succeeded, want a validation error", spec)
		}
	}
}

func TestExportResources_SnapshotState(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	id, _ := x.CreateSnapshot(context.Background(), logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "v"})

	st, err := x.SnapshotState(context.Background(), id)
	if err != nil || st.State != "pending" || st.Progress != "42%" {
		t.Fatalf("pending: %+v %v", st, err)
	}
	api.snaps[0].state = ec2types.SnapshotStateCompleted
	if st, _ := x.SnapshotState(context.Background(), id); st.State != "completed" {
		t.Fatalf("completed: %+v", st)
	}
	// An unknown snapshot is distinguishable from a failed describe: the FSM
	// treats the first as a vanished snapshot (fail the attempt), the second as
	// transient (retry).
	if _, err := x.SnapshotState(context.Background(), "snap-nope"); !errors.Is(err, logdb.ErrSnapshotNotFound) {
		t.Fatalf("SnapshotState of an unknown snapshot = %v, want logdb.ErrSnapshotNotFound", err)
	}
}

// ── instances ───────────────────────────────────────────────────────

func exportSpec() logdb.ExportSpec {
	return logdb.ExportSpec{ShardID: "s-1", Attempt: 2, SnapshotID: "snap-7", InstanceType: "r7g.xlarge", AZ: "us-east-2b", Prefix: "s3://weft-cold/warp"}
}

func TestExportResources_LaunchInstanceRequestShape(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)

	id, err := x.LaunchInstance(context.Background(), exportSpec())
	if err != nil || id == "" {
		t.Fatalf("LaunchInstance: %q %v", id, err)
	}
	in := api.runInputs[0]

	if aws.ToString(in.LaunchTemplate.LaunchTemplateId) != "lt-export" {
		t.Errorf("launch template = %v", in.LaunchTemplate)
	}
	if string(in.InstanceType) != "r7g.xlarge" {
		t.Errorf("instance type = %q", in.InstanceType)
	}
	if aws.ToString(in.ImageId) != "ami-coord" {
		t.Errorf("image = %q, want the coordinator's AMI so the export subcommand exists", aws.ToString(in.ImageId))
	}
	if aws.ToString(in.SubnetId) != "subnet-b" {
		t.Errorf("subnet = %q, want the one for us-east-2b", aws.ToString(in.SubnetId))
	}
	if in.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorTerminate {
		t.Errorf("shutdown behavior = %q, want terminate so a finished export host cannot linger", in.InstanceInitiatedShutdownBehavior)
	}

	if len(in.BlockDeviceMappings) != 1 {
		t.Fatalf("block device mappings = %+v", in.BlockDeviceMappings)
	}
	bd := in.BlockDeviceMappings[0]
	if aws.ToString(bd.DeviceName) != "/dev/sdf" || aws.ToString(bd.Ebs.SnapshotId) != "snap-7" ||
		!aws.ToBool(bd.Ebs.DeleteOnTermination) || bd.Ebs.VolumeType != ec2types.VolumeTypeGp3 {
		t.Errorf("data volume mapping = %+v / %+v", bd, bd.Ebs)
	}

	itags := tagMap(in.TagSpecifications, ec2types.ResourceTypeInstance)
	if itags["Role"] != "export" {
		t.Errorf("Role tag = %q: the AMI starts weft-$Role, and it must never be a shard", itags["Role"])
	}
	if itags["export_shard_id"] != "s-1" || itags["export_attempt"] != "2" || itags["export_prefix"] != "s3://weft-cold/warp" {
		t.Errorf("instance tags = %v", itags)
	}
	for _, key := range []string{"shard_id", "shard_tier"} {
		if _, has := itags[key]; has {
			t.Errorf("instance carries %q, which shard reconciliation keys on: %v", key, itags)
		}
	}
	vtags := tagMap(in.TagSpecifications, ec2types.ResourceTypeVolume)
	if vtags["export_shard_id"] != "s-1" {
		t.Errorf("volume tags = %v, want the export tags so the sweep can find it", vtags)
	}
}

func TestExportResources_LaunchInstanceIsIdempotentPerAttempt(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	id1, _ := x.LaunchInstance(context.Background(), exportSpec())
	id2, err := x.LaunchInstance(context.Background(), exportSpec())
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 || len(api.runInputs) != 1 {
		t.Fatalf("replay launched a second instance: %q vs %q, runs=%d", id1, id2, len(api.runInputs))
	}

	next := exportSpec()
	next.Attempt = 3
	id3, _ := x.LaunchInstance(context.Background(), next)
	if id3 == id1 {
		t.Fatal("attempt 3 adopted attempt 2's instance")
	}

	// A terminated instance from the same attempt is not adopted.
	api.insts[0].state = "terminated"
	id4, _ := x.LaunchInstance(context.Background(), exportSpec())
	if id4 == id1 {
		t.Fatal("adopted a terminated instance")
	}
}

func TestExportResources_LaunchInstanceFailsFastOnBadConfig(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)

	badAZ := exportSpec()
	badAZ.AZ = "us-east-2z"
	if _, err := x.LaunchInstance(context.Background(), badAZ); err == nil {
		t.Error("launch in an AZ with no subnet succeeded")
	}
	for name, mutate := range map[string]func(*logdb.ExportSpec){
		"no shard":    func(s *logdb.ExportSpec) { s.ShardID = "" },
		"no attempt":  func(s *logdb.ExportSpec) { s.Attempt = 0 },
		"no snapshot": func(s *logdb.ExportSpec) { s.SnapshotID = "" },
		"no type":     func(s *logdb.ExportSpec) { s.InstanceType = "" },
		"no prefix":   func(s *logdb.ExportSpec) { s.Prefix = "" },
	} {
		spec := exportSpec()
		mutate(&spec)
		if _, err := x.LaunchInstance(context.Background(), spec); err == nil {
			t.Errorf("%s: launch succeeded", name)
		}
	}
	noLT := NewExportResourcesWithClient(api, map[string]string{"us-east-2b": "subnet-b"}, "", "ami")
	if _, err := noLT.LaunchInstance(context.Background(), exportSpec()); err == nil {
		t.Error("launch with no launch template configured succeeded")
	}
	if len(api.runInputs) != 0 {
		t.Fatalf("RunInstances called %d times for invalid input", len(api.runInputs))
	}
}

func TestExportResources_InstanceState(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	id, _ := x.LaunchInstance(context.Background(), exportSpec())

	if st, err := x.InstanceState(context.Background(), id); err != nil || st != "pending" {
		t.Fatalf("state = %q %v", st, err)
	}
	api.insts[0].state = "terminated"
	if st, _ := x.InstanceState(context.Background(), id); st != "terminated" {
		t.Fatalf("state = %q", st)
	}
	if st, err := x.InstanceState(context.Background(), "i-nope"); err != nil || st != "" {
		t.Fatalf("unknown instance: state=%q err=%v (want empty, no error: it is gone)", st, err)
	}
}

// ── cleanup ─────────────────────────────────────────────────────────

func TestExportResources_CleanupRemovesOnlyThisShardsResources(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	ctx := context.Background()

	mine := exportSpec()
	other := exportSpec()
	other.ShardID = "s-other"
	mineSnap, _ := x.CreateSnapshot(ctx, logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "v"})
	otherSnap, _ := x.CreateSnapshot(ctx, logdb.SnapshotSpec{ShardID: "s-other", Attempt: 1, VolumeID: "v2"})
	mineInst, _ := x.LaunchInstance(ctx, mine)
	otherInst, _ := x.LaunchInstance(ctx, other)

	if err := x.Cleanup(ctx, "s-1"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(api.terminated) != 1 || api.terminated[0] != mineInst {
		t.Fatalf("terminated = %v, want only %s", api.terminated, mineInst)
	}
	if len(api.deletedSnapshots) != 1 || api.deletedSnapshots[0] != mineSnap {
		t.Fatalf("deleted snapshots = %v, want only %s", api.deletedSnapshots, mineSnap)
	}
	if st, _ := x.InstanceState(ctx, otherInst); st != "pending" {
		t.Fatalf("other shard's instance state = %q", st)
	}
	if _, err := x.SnapshotState(ctx, otherSnap); err != nil {
		t.Fatalf("other shard's snapshot gone: %v", err)
	}

	// Idempotent: a second cleanup finds nothing and succeeds.
	if err := x.Cleanup(ctx, "s-1"); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}

func TestExportResources_CleanupRequiresShardID(t *testing.T) {
	if err := newTestExportResources(&fakeExportEC2{}).Cleanup(context.Background(), ""); err == nil {
		t.Fatal("Cleanup with an empty shard id would match every export resource")
	}
}
