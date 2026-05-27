//go:build integration && localstack

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/kevlarcon/weft/logdb"
)

// These tests exercise the ASG-as-API launch primitive
// (logdb.ASGProvisioner.Launch) and the EBS VolumeManager against
// LocalStack's real EC2 implementation. They replace the old SNS/SQS
// lifecycle-hook tests, whose subsystem was deleted when the shard FSM
// took ownership of lifecycle (commit 7b0de94).
//
// LocalStack Community doesn't implement Auto Scaling, so the ASG side
// is faked (fakeASG below) — but everything the integration value lives
// in is real: RunInstances, CreateTags, CreateVolume, AttachVolume,
// DescribeVolumes, CreateSnapshot, DeleteVolume all hit LocalStack EC2.
// Launch is store-free post-7b0de94 (the shard SM persists the
// LaunchResult; that handoff is unit-tested in logdb/shardsm_test.go),
// so these tests need no postgres.

// LocalStack endpoint from docker-compose. Set LOCALSTACK_ENDPOINT to
// override; otherwise defaults to the docker-compose mapping.
const localStackEndpoint = "http://localhost:4566"

// awsTestConfig returns an aws.Config wired to LocalStack with throwaway
// credentials.
func awsTestConfig(t *testing.T) aws.Config {
	t.Helper()
	endpoint := os.Getenv("LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		endpoint = localStackEndpoint
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-2"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		awsconfig.WithBaseEndpoint(endpoint),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return cfg
}

// fakeASG stands in for AWS Auto Scaling, which LocalStack Community
// doesn't implement. It satisfies logdb.ASGApi. On SetDesiredCapacity
// it launches real LocalStack EC2 instances in its AZ — so the
// provisioner's subsequent CreateTags and AttachVolume calls land on a
// genuine instance — and reports every launched instance as InService
// immediately, which is all ASGProvisioner.awaitNewInstance needs.
type fakeASG struct {
	ec2 *ec2.Client
	az  string

	mu         sync.Mutex
	instances  []string
	desired    int32
	terminated []string
}

func (f *fakeASG) SetDesiredCapacity(ctx context.Context, in *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var want int32
	if in.DesiredCapacity != nil {
		want = *in.DesiredCapacity
	}
	for int32(len(f.instances)) < want {
		out, err := f.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
			MaxCount:     aws.Int32(1),
			MinCount:     aws.Int32(1),
			InstanceType: ec2types.InstanceTypeT3Micro,
			ImageId:      aws.String("ami-test"),
			Placement:    &ec2types.Placement{AvailabilityZone: aws.String(f.az)},
		})
		if err != nil {
			return nil, err
		}
		f.instances = append(f.instances, *out.Instances[0].InstanceId)
	}
	f.desired = want
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (f *fakeASG) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	insts := make([]astypes.Instance, 0, len(f.instances))
	for _, id := range f.instances {
		insts = append(insts, astypes.Instance{
			InstanceId:     aws.String(id),
			LifecycleState: astypes.LifecycleStateInService,
		})
	}
	name := ""
	if len(in.AutoScalingGroupNames) > 0 {
		name = in.AutoScalingGroupNames[0]
	}
	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []astypes.AutoScalingGroup{{
			AutoScalingGroupName: aws.String(name),
			DesiredCapacity:      aws.Int32(f.desired),
			Instances:            insts,
		}},
	}, nil
}

func (f *fakeASG) TerminateInstanceInAutoScalingGroup(_ context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.InstanceId != nil {
		f.terminated = append(f.terminated, *in.InstanceId)
	}
	return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
}

// newLaunchProvisioner wires an ASGProvisioner around the fake ASG and a
// real LocalStack-backed VolumeManager + tagger, configured for one
// (tier, AZ). Returns the provisioner and the EC2 client for assertions.
func newLaunchProvisioner(t *testing.T, az string) (*logdb.ASGProvisioner, *ec2.Client) {
	t.Helper()
	cfg := awsTestConfig(t)
	ec2c := ec2.NewFromConfig(cfg)
	vm := logdb.NewEC2VolumeManagerWithClient(ec2c)
	asgName := "weft-shard-small-" + testSuffix()
	asgs := map[string]map[string]string{"small": {az: asgName}}
	prov := logdb.NewASGProvisionerWithClients(&fakeASG{ec2: ec2c, az: az}, ec2c, vm, asgs)
	return prov, ec2c
}

// describeVolume returns the (raw) volume record for assertions.
func describeVolume(t *testing.T, ec2c *ec2.Client, volumeID string) ec2types.Volume {
	t.Helper()
	out, err := ec2c.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	})
	if err != nil {
		t.Fatalf("DescribeVolumes %s: %v", volumeID, err)
	}
	if len(out.Volumes) == 0 {
		t.Fatalf("volume %s not found", volumeID)
	}
	return out.Volumes[0]
}

func volumeTags(v ec2types.Volume) map[string]string {
	tags := map[string]string{}
	for _, tag := range v.Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}
	return tags
}

// testSuffix returns a short unique string so concurrently-running
// tests (or re-runs against the same LocalStack instance) don't
// collide on resource names/tags.
func testSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
}

// ── Tests ────────────────────────────────────────────────────

// Christen: a fresh Launch (ExistingVolumeID empty) brings up an
// instance, creates a new gp3 volume tagged with the shard id/tier, and
// attaches it at /dev/sdf. The ASG-as-API replacement for the old
// SNS-event → handler → create+attach path.
func TestLifecycle_Integration_ChristenPath(t *testing.T) {
	az := "us-east-2a"
	prov, ec2c := newLaunchProvisioner(t, az)
	shardID := "shard-christen-" + testSuffix()

	result, err := prov.Launch(context.Background(), logdb.LaunchSpec{
		ShardID: shardID,
		Tier:    "small",
		AZ:      az,
		SizeGB:  10, // fresh launch → create a volume
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.InstanceID == "" || result.VolumeID == "" {
		t.Fatalf("Launch returned empty ids: %+v", result)
	}
	if result.AZ != az {
		t.Errorf("LaunchResult.AZ: got %q want %q", result.AZ, az)
	}
	t.Cleanup(func() {
		_, _ = ec2c.DetachVolume(context.Background(), &ec2.DetachVolumeInput{VolumeId: aws.String(result.VolumeID), Force: aws.Bool(true)})
		_, _ = ec2c.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: aws.String(result.VolumeID)})
	})

	// The created volume carries the shard tags and is attached to the
	// launched instance at /dev/sdf (Launch blocks until in-use).
	v := describeVolume(t, ec2c, result.VolumeID)
	if v.State != ec2types.VolumeStateInUse {
		t.Errorf("volume state: got %q want in-use", v.State)
	}
	tags := volumeTags(v)
	if tags["shard_id"] != shardID {
		t.Errorf("volume shard_id tag: got %q want %q", tags["shard_id"], shardID)
	}
	if tags["shard_tier"] != "small" {
		t.Errorf("volume shard_tier tag: got %q want small", tags["shard_tier"])
	}
	if len(v.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(v.Attachments))
	}
	att := v.Attachments[0]
	if att.InstanceId == nil || *att.InstanceId != result.InstanceID {
		t.Errorf("attachment instance: got %v want %s", att.InstanceId, result.InstanceID)
	}
	if att.Device == nil || !strings.HasSuffix(*att.Device, "sdf") {
		t.Errorf("attachment device: got %v want .../sdf", att.Device)
	}

	// The instance was tagged with the shard id.
	di, err := ec2c.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{
		InstanceIds: []string{result.InstanceID},
	})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	instTag := ""
	for _, r := range di.Reservations {
		for _, inst := range r.Instances {
			for _, tag := range inst.Tags {
				if tag.Key != nil && *tag.Key == "shard_id" && tag.Value != nil {
					instTag = *tag.Value
				}
			}
		}
	}
	if instTag != shardID {
		t.Errorf("instance shard_id tag: got %q want %q", instTag, shardID)
	}
}

// Recover: a Launch with ExistingVolumeID set reattaches the surviving
// volume to a fresh instance instead of creating a new one — the path
// the shard SM takes after an instance dies (it carries the volume id
// in shard_records.ebs_volume_id and passes it back to Launch).
func TestLifecycle_Integration_RecoverPath(t *testing.T) {
	az := "us-east-2a"
	prov, ec2c := newLaunchProvisioner(t, az)
	shardID := "shard-recover-" + testSuffix()

	// Pre-seed the surviving volume, tagged as the shard's data volume.
	volOut, err := ec2c.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int32(10),
		VolumeType:       ec2types.VolumeTypeGp3,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVolume,
			Tags: []ec2types.Tag{
				{Key: aws.String("shard_id"), Value: aws.String(shardID)},
				{Key: aws.String("shard_tier"), Value: aws.String("small")},
			},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume (surviving): %v", err)
	}
	survivingID := *volOut.VolumeId
	t.Cleanup(func() {
		_, _ = ec2c.DetachVolume(context.Background(), &ec2.DetachVolumeInput{VolumeId: aws.String(survivingID), Force: aws.Bool(true)})
		_, _ = ec2c.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: aws.String(survivingID)})
	})

	result, err := prov.Launch(context.Background(), logdb.LaunchSpec{
		ShardID:          shardID,
		Tier:             "small",
		AZ:               az,
		ExistingVolumeID: survivingID, // recovery → reattach, don't create
	})
	if err != nil {
		t.Fatalf("Launch (recovery): %v", err)
	}
	if result.VolumeID != survivingID {
		t.Errorf("recovery attached wrong volume: got %s want %s (the survivor)", result.VolumeID, survivingID)
	}

	// No *new* volume was created for shardID — exactly one volume is
	// tagged shard_id=<shardID>, the survivor we pre-seeded.
	volumes, err := ec2c.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:shard_id"), Values: []string{shardID}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(volumes.Volumes); n != 1 {
		t.Errorf("expected exactly 1 volume tagged shard_id=%s, got %d", shardID, n)
	}

	// The survivor is attached to the recovery instance at /dev/sdf.
	v := describeVolume(t, ec2c, survivingID)
	if len(v.Attachments) != 1 || v.Attachments[0].InstanceId == nil || *v.Attachments[0].InstanceId != result.InstanceID {
		t.Errorf("survivor not attached to recovery instance %s: attachments=%+v", result.InstanceID, v.Attachments)
	}
}

// Retirement: the archive path snapshots a shard's volume (tagged with
// shard_id + parquet_path for lineage), then deletes it. Exercises the
// real VolumeManager SnapshotVolume/DeleteVolume calls — unchanged by
// the ASG-as-API switch (the shard SM's evaluateArchiving drives this
// same sequence in production).
func TestLifecycle_Integration_RetirementPath(t *testing.T) {
	cfg := awsTestConfig(t)
	ec2c := ec2.NewFromConfig(cfg)

	shardID := "shard-retire-" + testSuffix()
	parquetPath := "s3://weft-cold/" + shardID + ".parquet"
	az := "us-east-2a"

	volOut, err := ec2c.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int32(10),
		VolumeType:       ec2types.VolumeTypeGp3,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVolume,
			Tags: []ec2types.Tag{
				{Key: aws.String("shard_id"), Value: aws.String(shardID)},
				{Key: aws.String("shard_tier"), Value: aws.String("small")},
			},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := *volOut.VolumeId

	vm := logdb.NewEC2VolumeManagerWithClient(ec2c)

	// Exercise the production sequence directly — snapshot → delete —
	// through the real SDK calls. Skips the full archive() wrapper
	// (which wants a DuckDBShard); this verifies the AWS side, not the
	// Parquet export.
	ctx := context.Background()
	snapID, err := vm.SnapshotVolume(ctx, logdb.SnapshotOpts{
		VolumeID:    volumeID,
		ShardID:     shardID,
		ParquetPath: parquetPath,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume: %v", err)
	}
	if snapID == "" {
		t.Fatal("empty snapshot id")
	}

	// Snapshot should carry shard_id + parquet_path tags so operators
	// can reconstruct the lineage later.
	snapOut, err := ec2c.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		SnapshotIds: []string{snapID},
	})
	if err != nil {
		t.Fatalf("DescribeSnapshots: %v", err)
	}
	if len(snapOut.Snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapOut.Snapshots))
	}
	tags := map[string]string{}
	for _, tag := range snapOut.Snapshots[0].Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}
	if tags["shard_id"] != shardID {
		t.Errorf("snapshot shard_id: got %q want %q", tags["shard_id"], shardID)
	}
	if tags["parquet_path"] != parquetPath {
		t.Errorf("snapshot parquet_path: got %q want %q", tags["parquet_path"], parquetPath)
	}

	// Volume isn't attached (we skipped attach above), so a non-force
	// detach would error — archive() always uses force=true for exactly
	// this reason. Move straight to delete.
	if err := vm.DeleteVolume(ctx, volumeID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	// Volume should be gone. EC2 often returns an InvalidVolume.NotFound
	// error for a deleted volume; that's the cleanest pass signal. If it
	// does return a record, it must be in deleted/deleting.
	volOut2, err := ec2c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	})
	if err != nil {
		if !strings.Contains(err.Error(), "NotFound") {
			t.Errorf("post-delete DescribeVolumes: unexpected error %v", err)
		}
		return
	}
	for _, v := range volOut2.Volumes {
		if v.State != ec2types.VolumeStateDeleted && v.State != ec2types.VolumeStateDeleting {
			t.Errorf("volume %s still in state %s after DeleteVolume", volumeID, v.State)
		}
	}
}
