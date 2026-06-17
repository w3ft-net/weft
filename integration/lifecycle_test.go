//go:build integration && localstack

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/graybeardcorp/weft/logdb"
)

// These tests exercise the EC2-direct launch primitive
// (logdb.EC2Provisioner.Launch) and the EBS VolumeManager against
// LocalStack's real EC2 implementation. Since the coordinator now launches
// instances itself via RunInstances/TerminateInstances — no Auto Scaling
// Group — there is nothing to fake: RunInstances, DescribeInstances (the
// idempotency find-by-tag + wait-for-running), CreateVolume, AttachVolume,
// DescribeVolumes, CreateSnapshot, DeleteVolume all hit LocalStack EC2.
// Launch is store-free (the shard SM persists the LaunchResult), so these
// tests need no postgres.

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

// subnetInAZ returns a subnet id in the given AZ (LocalStack seeds default
// subnets per AZ in the default VPC).
func subnetInAZ(t *testing.T, ec2c *ec2.Client, az string) string {
	t.Helper()
	out, err := ec2c.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("availability-zone"), Values: []string{az}}},
	})
	if err != nil {
		t.Fatalf("DescribeSubnets %s: %v", az, err)
	}
	if len(out.Subnets) == 0 || out.Subnets[0].SubnetId == nil {
		t.Fatalf("no subnet found in %s", az)
	}
	return *out.Subnets[0].SubnetId
}

// createLaunchTemplate makes a minimal launch template the provisioner runs
// shard instances from (the prod template's AMI/IAM/SG aren't needed to
// verify the launch+attach+tag wiring).
func createLaunchTemplate(t *testing.T, ec2c *ec2.Client) string {
	t.Helper()
	out, err := ec2c.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("weft-shard-small-" + testSuffix()),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{
			ImageId:      aws.String("ami-test"),
			InstanceType: ec2types.InstanceTypeT3Micro,
		},
	})
	if err != nil {
		t.Fatalf("CreateLaunchTemplate: %v", err)
	}
	return *out.LaunchTemplate.LaunchTemplateId
}

// newLaunchProvisioner wires an EC2Provisioner against LocalStack EC2 + a
// real LocalStack-backed VolumeManager, configured for one (tier, AZ).
func newLaunchProvisioner(t *testing.T, az string) (*logdb.EC2Provisioner, *ec2.Client) {
	t.Helper()
	cfg := awsTestConfig(t)
	ec2c := ec2.NewFromConfig(cfg)
	vm := logdb.NewEC2VolumeManagerWithClient(ec2c)
	prov := logdb.NewEC2ProvisionerWithClient(ec2c, vm,
		map[string]map[string]string{"small": {az: subnetInAZ(t, ec2c, az)}},
		map[string]string{"small": createLaunchTemplate(t, ec2c)},
		"", // no AMI override under LocalStack: use the launch template's image
	)
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

// testSuffix returns a short unique string so concurrently-running tests (or
// re-runs against the same LocalStack instance) don't collide on resource
// names/tags.
func testSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
}

// ── Tests ────────────────────────────────────────────────────

// Christen: a fresh Launch (ExistingVolumeID empty) runs an instance tagged
// with the shard id at birth, creates a new gp3 volume tagged shard_id/tier,
// and attaches it at /dev/sdf.
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
		_, _ = ec2c.TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{InstanceIds: []string{result.InstanceID}})
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

	// The instance was tagged shard_id at birth (RunInstances
	// TagSpecifications) — never momentarily untagged.
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

// Recover: a Launch with ExistingVolumeID set reattaches the surviving volume
// to a fresh instance instead of creating a new one — the path the shard SM
// takes after an instance dies (it carries the volume id in
// shard_records.ebs_volume_id and passes it back to Launch).
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
	t.Cleanup(func() {
		_, _ = ec2c.TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{InstanceIds: []string{result.InstanceID}})
	})
	if result.VolumeID != survivingID {
		t.Errorf("recovery attached wrong volume: got %s want %s (the survivor)", result.VolumeID, survivingID)
	}

	// No *new* volume was created for shardID — exactly one volume is tagged
	// shard_id=<shardID>, the survivor we pre-seeded.
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
// shard_id + parquet_path for lineage), then deletes it. Exercises the real
// VolumeManager SnapshotVolume/DeleteVolume calls — unchanged by the
// EC2-direct switch (the shard SM's evaluateArchiving drives this same
// sequence in production).
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

	// Snapshot should carry shard_id + parquet_path tags so operators can
	// reconstruct the lineage later.
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

	if err := vm.DeleteVolume(ctx, volumeID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	// Volume should be gone. EC2 often returns InvalidVolume.NotFound for a
	// deleted volume; that's the cleanest pass signal. If it does return a
	// record, it must be in deleted/deleting.
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
