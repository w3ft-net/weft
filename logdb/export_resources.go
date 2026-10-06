package logdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Ephemeral archive export resources (docs/plans/ephemeral-archive-export.md):
// a snapshot of the shard's volume, and a short-lived instance that boots with
// a volume restored from it and runs `weft-shard export`.
//
// Every resource is tagged export_shard_id / export_attempt and NEVER shard_id
// or shard_tier. The shard lookups (EC2Provisioner.findLiveByShard,
// EC2VolumeManager.FindByShard) key on shard_id, so an export instance or
// volume can never be mistaken for the shard's own — the ghost-instance class.
// The instance carries Role=export: the AMI's startup.sh starts weft-$Role, so
// the export host runs weft-export.service, not weft-shard.
//
// Each method is a single non-blocking AWS interaction, idempotent by tag. The
// shard FSM polls SnapshotState / InstanceState across ticks instead of
// waiting inside a call.
const (
	exportTagShard   = "export_shard_id"
	exportTagAttempt = "export_attempt"
	exportTagPrefix  = "export_prefix"

	// exportRole is the Role tag value of an export instance.
	exportRole = "export"

	// exportDevice is where the restored volume attaches; startup.sh finds it
	// by this requested name, as it does for a shard's data volume.
	exportDevice = "/dev/sdf"
)

// EC2ExportAPI is the slice of the EC2 client ExportResources needs.
type EC2ExportAPI interface {
	CreateSnapshot(ctx context.Context, in *ec2.CreateSnapshotInput, opts ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error)
	DescribeSnapshots(ctx context.Context, in *ec2.DescribeSnapshotsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
	DeleteSnapshot(ctx context.Context, in *ec2.DeleteSnapshotInput, opts ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// SnapshotSpec describes the snapshot of a shard's volume for one export attempt.
type SnapshotSpec struct {
	ShardID  string
	Attempt  int
	VolumeID string
}

// ExportSnapshotState is a snapshot's progress as EC2 reports it.
type ExportSnapshotState struct {
	State    string // pending | completed | error | recoverable
	Progress string // e.g. "42%"
}

// ExportSpec describes the instance for one export attempt.
type ExportSpec struct {
	ShardID      string
	Attempt      int
	SnapshotID   string
	InstanceType string
	AZ           string // must have a configured subnet
	Prefix       string // archive prefix the export writes under
}

// ExportResources creates, observes and removes the per-attempt export
// snapshot and instance.
type ExportResources struct {
	ec2 EC2ExportAPI

	// subnets maps az → subnet id for export instances.
	subnets        map[string]string
	launchTemplate string

	// imageID is the coordinator's own AMI, so the export host runs the same
	// build as the coordinator — and therefore has the export subcommand. ""
	// falls back to the launch template's AMI.
	imageID string
}

// NewExportResources builds an EC2-backed ExportResources. subnets maps az →
// subnet id; launchTemplate is the dedicated export launch template (Role=export,
// IMDS tags enabled, an instance profile that can write the archive prefix).
func NewExportResources(region string, subnets map[string]string, launchTemplate string) (*ExportResources, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	imageID, err := resolveOwnImageID(context.Background())
	if err != nil {
		log.Printf("export-resources: could not resolve own AMI from IMDS (%v); export launches will use the launch template AMI", err)
		imageID = ""
	}
	return NewExportResourcesWithClient(ec2.NewFromConfig(cfg), subnets, launchTemplate, imageID), nil
}

// NewExportResourcesWithClient injects the EC2 client (for tests).
func NewExportResourcesWithClient(client EC2ExportAPI, subnets map[string]string, launchTemplate, imageID string) *ExportResources {
	return &ExportResources{ec2: client, subnets: subnets, launchTemplate: launchTemplate, imageID: imageID}
}

func attemptTagFilters(shardID string, attempt int) []ec2types.Filter {
	return []ec2types.Filter{
		{Name: aws.String("tag:" + exportTagShard), Values: []string{shardID}},
		{Name: aws.String("tag:" + exportTagAttempt), Values: []string{strconv.Itoa(attempt)}},
	}
}

// CreateSnapshot snapshots spec.VolumeID for the attempt and returns the
// snapshot id without waiting for it to complete (poll SnapshotState).
// Idempotent: a snapshot already tagged for this shard and attempt is adopted,
// unless it is in the error state, in which case a fresh one is created.
func (x *ExportResources) CreateSnapshot(ctx context.Context, spec SnapshotSpec) (string, error) {
	if spec.ShardID == "" || spec.Attempt <= 0 || spec.VolumeID == "" {
		return "", fmt.Errorf("CreateSnapshot: shard id, attempt > 0 and volume id are required (got %+v)", spec)
	}
	existing, err := x.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
		Filters:  attemptTagFilters(spec.ShardID, spec.Attempt),
	})
	if err != nil {
		return "", fmt.Errorf("find export snapshot for %s attempt %d: %w", spec.ShardID, spec.Attempt, err)
	}
	for _, s := range existing.Snapshots {
		if s.SnapshotId != nil && s.State != ec2types.SnapshotStateError {
			log.Printf("export-resources: adopting snapshot %s already tagged for %s attempt %d", *s.SnapshotId, spec.ShardID, spec.Attempt)
			return *s.SnapshotId, nil
		}
	}

	name := fmt.Sprintf("%s-export-%d", spec.ShardID, spec.Attempt)
	out, err := x.ec2.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId:    aws.String(spec.VolumeID),
		Description: aws.String("weft archive export of " + spec.ShardID),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeSnapshot,
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String(name)},
				{Key: aws.String(exportTagShard), Value: aws.String(spec.ShardID)},
				{Key: aws.String(exportTagAttempt), Value: aws.String(strconv.Itoa(spec.Attempt))},
			},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("CreateSnapshot of %s for %s: %w", spec.VolumeID, spec.ShardID, err)
	}
	if out.SnapshotId == nil {
		return "", fmt.Errorf("CreateSnapshot of %s: no id returned", spec.VolumeID)
	}
	return *out.SnapshotId, nil
}

// SnapshotState reports a snapshot's state. An unknown snapshot is an error.
func (x *ExportResources) SnapshotState(ctx context.Context, snapshotID string) (ExportSnapshotState, error) {
	out, err := x.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{snapshotID}})
	if err != nil {
		return ExportSnapshotState{}, fmt.Errorf("describe snapshot %s: %w", snapshotID, err)
	}
	if len(out.Snapshots) == 0 {
		return ExportSnapshotState{}, fmt.Errorf("snapshot %s not found", snapshotID)
	}
	s := out.Snapshots[0]
	return ExportSnapshotState{State: string(s.State), Progress: aws.ToString(s.Progress)}, nil
}

// LaunchInstance starts the export instance for the attempt with a volume
// restored from spec.SnapshotID attached at exportDevice, and returns its id
// without waiting for it to run (poll InstanceState). The volume is created and
// attached by RunInstances itself and deleted when the instance terminates, so
// there is no separate export volume to track or leak. The instance terminates,
// not stops, when it shuts itself down. Idempotent: a pending|running instance
// already tagged for this shard and attempt is adopted.
func (x *ExportResources) LaunchInstance(ctx context.Context, spec ExportSpec) (string, error) {
	if spec.ShardID == "" || spec.Attempt <= 0 || spec.SnapshotID == "" || spec.InstanceType == "" || spec.Prefix == "" {
		return "", fmt.Errorf("LaunchInstance: shard id, attempt > 0, snapshot id, instance type and prefix are required (got %+v)", spec)
	}
	subnet, ok := x.subnets[spec.AZ]
	if !ok {
		return "", fmt.Errorf("LaunchInstance %s: no export subnet configured for az %q", spec.ShardID, spec.AZ)
	}
	if x.launchTemplate == "" {
		return "", fmt.Errorf("LaunchInstance %s: no export launch template configured", spec.ShardID)
	}

	found, err := x.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: append(attemptTagFilters(spec.ShardID, spec.Attempt),
			ec2types.Filter{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}}),
	})
	if err != nil {
		return "", fmt.Errorf("find export instance for %s attempt %d: %w", spec.ShardID, spec.Attempt, err)
	}
	for _, r := range found.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceId != nil {
				log.Printf("export-resources: adopting in-flight export instance %s for %s attempt %d", *inst.InstanceId, spec.ShardID, spec.Attempt)
				return *inst.InstanceId, nil
			}
		}
	}

	attempt := strconv.Itoa(spec.Attempt)
	exportTags := []ec2types.Tag{
		{Key: aws.String(exportTagShard), Value: aws.String(spec.ShardID)},
		{Key: aws.String(exportTagAttempt), Value: aws.String(attempt)},
		{Key: aws.String(exportTagPrefix), Value: aws.String(spec.Prefix)},
	}
	in := &ec2.RunInstancesInput{
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(x.launchTemplate),
			Version:          aws.String("$Latest"),
		},
		InstanceType: ec2types.InstanceType(spec.InstanceType),
		SubnetId:     aws.String(subnet),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String(exportDevice),
			Ebs: &ec2types.EbsBlockDevice{
				SnapshotId:          aws.String(spec.SnapshotID),
				VolumeType:          ec2types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true),
			},
		}},
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: append([]ec2types.Tag{
					{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("%s-export-%s", spec.ShardID, attempt))},
					{Key: aws.String("Role"), Value: aws.String(exportRole)},
				}, exportTags...),
			},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: exportTags},
		},
	}
	if x.imageID != "" {
		in.ImageId = aws.String(x.imageID)
	}
	out, err := x.ec2.RunInstances(ctx, in)
	if err != nil {
		return "", fmt.Errorf("RunInstances export for %s (type=%s az=%s snapshot=%s): %w", spec.ShardID, spec.InstanceType, spec.AZ, spec.SnapshotID, err)
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		return "", fmt.Errorf("RunInstances export for %s: no instance returned", spec.ShardID)
	}
	return *out.Instances[0].InstanceId, nil
}

// InstanceState returns the instance's state name (pending, running,
// shutting-down, terminated, ...), or "" if EC2 no longer knows it.
func (x *ExportResources) InstanceState(ctx context.Context, instanceID string) (string, error) {
	out, err := x.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		if instanceAlreadyGone(err) {
			return "", nil
		}
		return "", fmt.Errorf("describe export instance %s: %w", instanceID, err)
	}
	return instanceState(out, instanceID), nil
}

// Cleanup terminates every live export instance and deletes every export
// snapshot tagged for shardID, across all attempts. The restored volumes go
// with their instances. It is idempotent, and it attempts everything even if
// one step fails, returning the errors joined.
func (x *ExportResources) Cleanup(ctx context.Context, shardID string) error {
	if shardID == "" {
		return fmt.Errorf("Cleanup: shard id required (an empty id would match every export resource)")
	}
	var errs []error
	tagFilter := ec2types.Filter{Name: aws.String("tag:" + exportTagShard), Values: []string{shardID}}

	insts, err := x.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{tagFilter, {
			Name:   aws.String("instance-state-name"),
			Values: []string{"pending", "running", "stopping", "stopped"},
		}},
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("list export instances for %s: %w", shardID, err))
	} else {
		for _, r := range insts.Reservations {
			for _, inst := range r.Instances {
				if inst.InstanceId == nil {
					continue
				}
				if _, err := x.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{*inst.InstanceId}}); err != nil && !instanceAlreadyGone(err) {
					errs = append(errs, fmt.Errorf("terminate %s: %w", *inst.InstanceId, err))
				}
			}
		}
	}

	snaps, err := x.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{OwnerIds: []string{"self"}, Filters: []ec2types.Filter{tagFilter}})
	if err != nil {
		errs = append(errs, fmt.Errorf("list export snapshots for %s: %w", shardID, err))
	} else {
		for _, s := range snaps.Snapshots {
			if s.SnapshotId == nil {
				continue
			}
			if _, err := x.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: s.SnapshotId}); err != nil && !snapshotAlreadyGone(err) {
				errs = append(errs, fmt.Errorf("delete snapshot %s: %w", *s.SnapshotId, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ExportResource is one export snapshot or instance found in AWS, with the
// identity its tags carry. Created is zero when EC2 did not report a time.
type ExportResource struct {
	Kind    string // "instance" or "snapshot"
	ID      string
	ShardID string
	Attempt int
	Created time.Time
}

// ListExportResources returns every live export instance and every export
// snapshot in the account, found by the export_shard_id tag. Instances that
// are already terminated are skipped: nothing is left to remove. It is the
// sweep's view of the world, independent of what the shard rows say.
func (x *ExportResources) ListExportResources(ctx context.Context) ([]ExportResource, error) {
	hasTag := ec2types.Filter{Name: aws.String("tag-key"), Values: []string{exportTagShard}}
	var out []ExportResource

	insts, err := x.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{hasTag, {
			Name:   aws.String("instance-state-name"),
			Values: []string{"pending", "running", "stopping", "stopped"},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("list export instances: %w", err)
	}
	for _, r := range insts.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceId == nil {
				continue
			}
			res := exportResourceFromTags("instance", *inst.InstanceId, inst.Tags)
			if inst.LaunchTime != nil {
				res.Created = *inst.LaunchTime
			}
			out = append(out, res)
		}
	}

	snaps, err := x.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"}, Filters: []ec2types.Filter{hasTag},
	})
	if err != nil {
		return nil, fmt.Errorf("list export snapshots: %w", err)
	}
	for _, s := range snaps.Snapshots {
		if s.SnapshotId == nil {
			continue
		}
		res := exportResourceFromTags("snapshot", *s.SnapshotId, s.Tags)
		if s.StartTime != nil {
			res.Created = *s.StartTime
		}
		out = append(out, res)
	}
	return out, nil
}

func exportResourceFromTags(kind, id string, tags []ec2types.Tag) ExportResource {
	r := ExportResource{Kind: kind, ID: id}
	for _, t := range tags {
		switch aws.ToString(t.Key) {
		case exportTagShard:
			r.ShardID = aws.ToString(t.Value)
		case exportTagAttempt:
			r.Attempt, _ = strconv.Atoi(aws.ToString(t.Value))
		}
	}
	return r
}

// DeleteExportResource removes one export instance or snapshot. A resource
// that is already gone is success: the sweep races the FSM's own cleanup.
func (x *ExportResources) DeleteExportResource(ctx context.Context, r ExportResource) error {
	switch r.Kind {
	case "instance":
		_, err := x.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{r.ID}})
		if err != nil && !instanceAlreadyGone(err) {
			return fmt.Errorf("terminate %s: %w", r.ID, err)
		}
	case "snapshot":
		_, err := x.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(r.ID)})
		if err != nil && !snapshotAlreadyGone(err) {
			return fmt.Errorf("delete snapshot %s: %w", r.ID, err)
		}
	default:
		return fmt.Errorf("unknown export resource kind %q (%s)", r.Kind, r.ID)
	}
	return nil
}

// snapshotAlreadyGone reports whether a delete error means the snapshot is
// already absent (a benign race we treat as success).
func snapshotAlreadyGone(err error) bool {
	return strings.Contains(err.Error(), "InvalidSnapshot.NotFound")
}
