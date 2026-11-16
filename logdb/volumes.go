package logdb

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2VolumesAPI is the subset of the EC2 client used by the volume
// manager. Defined as an interface so tests can inject a mock.
type EC2VolumesAPI interface {
	CreateVolume(ctx context.Context, in *ec2.CreateVolumeInput, opts ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error)
	DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, opts ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	AttachVolume(ctx context.Context, in *ec2.AttachVolumeInput, opts ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error)
	DetachVolume(ctx context.Context, in *ec2.DetachVolumeInput, opts ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error)
	DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, opts ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

// VolumeInfo is a flattened view of an EBS volume relevant to shard
// lifecycle decisions.
type VolumeInfo struct {
	VolumeID string
	AZ       string
	State    string // available / in-use / creating / deleting / ...
	ShardID  string // from the shard_id tag; empty for volumes created pre-christening
	Tier     string // from the shard_tier tag
	// InstanceID is the instance the volume is attached (or attaching) to,
	// "" when it is available. An upgrade's swap and rollback re-derive
	// their progress from it.
	InstanceID string
}

// CreateVolumeOpts parameterises a CreateForShard call.
type CreateVolumeOpts struct {
	ShardID string
	Tier    string // small / medium / large; stored as shard_tier tag
	AZ      string
	SizeGB  int32
}

// VolumeManager wraps the EC2 volume APIs with a narrow
// interface the coordinator can use without dragging the full SDK
// into every call site. Pure wrappers — decisions about which call
// to make (christen vs recover, archive vs retain) live in the
// provisioner, not here.
type VolumeManager interface {
	CreateForShard(ctx context.Context, opts CreateVolumeOpts) (volumeID string, err error)
	FindByShard(ctx context.Context, shardID string) (*VolumeInfo, error)
	ListAvailableByTier(ctx context.Context, tier, az string) ([]VolumeInfo, error)
	TagWithShardID(ctx context.Context, volumeID, shardID string) error
	AttachToInstance(ctx context.Context, volumeID, instanceID, device string) error
	DetachFromInstance(ctx context.Context, volumeID string, force bool) error
	DeleteVolume(ctx context.Context, volumeID string) error
}

// EC2VolumeManager is the production VolumeManager over aws-sdk-go-v2.
type EC2VolumeManager struct {
	client       EC2VolumesAPI
	pollInterval time.Duration
	pollAttempts int
}

// NewEC2VolumeManager builds a real EC2-backed VolumeManager.
func NewEC2VolumeManager(region string) (*EC2VolumeManager, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return NewEC2VolumeManagerWithClient(ec2.NewFromConfig(cfg)), nil
}

// NewEC2VolumeManagerWithClient injects an EC2 client. Used by tests
// via mockEC2Volumes.
func NewEC2VolumeManagerWithClient(client EC2VolumesAPI) *EC2VolumeManager {
	return &EC2VolumeManager{
		client: client,
		// 100ms: an upgrade's detach and attach sit inside the window in
		// which callers hold. DescribeVolumes at 10/s per wait stays well
		// inside EC2's request budget, and a throttled poll just retries.
		pollInterval: 100 * time.Millisecond,
		pollAttempts: 1200, // up to ~2 min — attach/detach can be slow under load
	}
}

// CreateForShard creates a gp3 volume in the given AZ, tagged with
// shard_id / shard_tier / Name. Returns the volume id. Does not wait
// for the volume to leave the "creating" state — attach polls for
// readiness separately.
func (m *EC2VolumeManager) CreateForShard(ctx context.Context, opts CreateVolumeOpts) (string, error) {
	size := opts.SizeGB
	if size <= 0 {
		return "", fmt.Errorf("CreateForShard: SizeGB must be > 0")
	}
	if opts.ShardID == "" {
		return "", fmt.Errorf("CreateForShard: ShardID required")
	}
	if opts.AZ == "" {
		return "", fmt.Errorf("CreateForShard: AZ required")
	}

	tagSpec := ec2types.TagSpecification{
		ResourceType: ec2types.ResourceTypeVolume,
		Tags: []ec2types.Tag{
			{Key: aws.String("Name"), Value: aws.String(opts.ShardID)},
			{Key: aws.String("shard_id"), Value: aws.String(opts.ShardID)},
			{Key: aws.String("shard_tier"), Value: aws.String(opts.Tier)},
		},
	}

	out, err := m.client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone:  aws.String(opts.AZ),
		Size:              aws.Int32(size),
		VolumeType:        ec2types.VolumeTypeGp3,
		TagSpecifications: []ec2types.TagSpecification{tagSpec},
	})
	if err != nil {
		return "", fmt.Errorf("CreateVolume %s: %w", opts.ShardID, err)
	}
	if out.VolumeId == nil {
		return "", fmt.Errorf("CreateVolume %s: no id returned", opts.ShardID)
	}
	return *out.VolumeId, nil
}

// FindByShard looks up the volume tagged with shard_id=<id>. Returns
// nil, nil when no such volume exists (christening case). Multiple
// matches return the first — the provisioner is responsible for
// surfacing a duplicates alarm via a separate code path.
func (m *EC2VolumeManager) FindByShard(ctx context.Context, shardID string) (*VolumeInfo, error) {
	if shardID == "" {
		return nil, fmt.Errorf("FindByShard: shardID required")
	}
	out, err := m.client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("tag:shard_id"),
			Values: []string{shardID},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeVolumes tag:shard_id=%s: %w", shardID, err)
	}
	if len(out.Volumes) == 0 {
		return nil, nil
	}
	v := out.Volumes[0]
	info := &VolumeInfo{}
	if v.VolumeId != nil {
		info.VolumeID = *v.VolumeId
	}
	if v.AvailabilityZone != nil {
		info.AZ = *v.AvailabilityZone
	}
	info.State = string(v.State)
	for _, a := range v.Attachments {
		if a.InstanceId == nil {
			continue
		}
		switch a.State {
		case ec2types.VolumeAttachmentStateAttached, ec2types.VolumeAttachmentStateAttaching, ec2types.VolumeAttachmentStateBusy:
			info.InstanceID = *a.InstanceId
		}
	}
	return info, nil
}

// ListAvailableByTier returns volumes in the given AZ that (a) carry
// the requested shard_tier tag and (b) are currently available (i.e.
// not attached to an instance). These are recovery candidates — their
// previous instance died and left the volume detached.
func (m *EC2VolumeManager) ListAvailableByTier(ctx context.Context, tier, az string) ([]VolumeInfo, error) {
	if tier == "" || az == "" {
		return nil, fmt.Errorf("ListAvailableByTier: tier and az required")
	}
	out, err := m.client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:shard_tier"), Values: []string{tier}},
			{Name: aws.String("availability-zone"), Values: []string{az}},
			{Name: aws.String("status"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeVolumes tier=%s az=%s: %w", tier, az, err)
	}
	infos := make([]VolumeInfo, 0, len(out.Volumes))
	for _, v := range out.Volumes {
		info := VolumeInfo{State: string(v.State)}
		if v.VolumeId != nil {
			info.VolumeID = *v.VolumeId
		}
		if v.AvailabilityZone != nil {
			info.AZ = *v.AvailabilityZone
		}
		for _, t := range v.Tags {
			if t.Key == nil || t.Value == nil {
				continue
			}
			switch *t.Key {
			case "shard_id":
				info.ShardID = *t.Value
			case "shard_tier":
				info.Tier = *t.Value
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// TagWithShardID stamps a volume with its shard id after christening
// minted the id. Used for volumes that were created before the shard
// had an identity (the common "brand-new shard" path).
func (m *EC2VolumeManager) TagWithShardID(ctx context.Context, volumeID, shardID string) error {
	if volumeID == "" || shardID == "" {
		return fmt.Errorf("TagWithShardID: volumeID and shardID required")
	}
	_, err := m.client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{volumeID},
		Tags: []ec2types.Tag{
			{Key: aws.String("shard_id"), Value: aws.String(shardID)},
			{Key: aws.String("Name"), Value: aws.String(shardID)},
		},
	})
	if err != nil {
		return fmt.Errorf("CreateTags on %s: %w", volumeID, err)
	}
	return nil
}

// AttachToInstance attaches volumeID to instanceID at the given
// device path. Waits for the volume to be `available` first —
// CreateVolume returns immediately with the volume still in
// `creating` state, and AttachVolume fails with IncorrectState if
// called too early. Then polls until DescribeVolumes reports
// `in-use`. Poll interval and attempts are exported (test shim).
func (m *EC2VolumeManager) AttachToInstance(ctx context.Context, volumeID, instanceID, device string) error {
	if err := m.waitForVolumeState(ctx, volumeID, ec2types.VolumeStateAvailable); err != nil {
		return fmt.Errorf("wait for %s available: %w", volumeID, err)
	}
	if _, err := m.client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(device),
	}); err != nil {
		return fmt.Errorf("AttachVolume %s → %s: %w", volumeID, instanceID, err)
	}
	return m.waitForVolumeState(ctx, volumeID, ec2types.VolumeStateInUse)
}

// DetachFromInstance detaches volumeID. If force=true, a stuck detach
// (from a dead instance) gets the force flag. Polls until the volume
// reports available.
func (m *EC2VolumeManager) DetachFromInstance(ctx context.Context, volumeID string, force bool) error {
	if _, err := m.client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId: aws.String(volumeID),
		Force:    aws.Bool(force),
	}); err != nil {
		return fmt.Errorf("DetachVolume %s: %w", volumeID, err)
	}
	return m.waitForVolumeState(ctx, volumeID, ec2types.VolumeStateAvailable)
}

// DeleteVolume removes the volume. Caller is responsible for
// snapshotting first if durability matters.
func (m *EC2VolumeManager) DeleteVolume(ctx context.Context, volumeID string) error {
	_, err := m.client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	})
	if err != nil {
		return fmt.Errorf("DeleteVolume %s: %w", volumeID, err)
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────

func (m *EC2VolumeManager) waitForVolumeState(ctx context.Context, volumeID string, want ec2types.VolumeState) error {
	for i := 0; i < m.pollAttempts; i++ {
		out, err := m.client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []string{volumeID},
		})
		if err == nil && len(out.Volumes) > 0 && out.Volumes[0].State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.pollInterval):
		}
	}
	return fmt.Errorf("volume %s did not reach state %s", volumeID, want)
}
