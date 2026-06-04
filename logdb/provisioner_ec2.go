package logdb

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// shardVolumeDevice is the block device the shard binary mounts for its
// DuckDB data (matches entrypoint.sh's mount of /dev/sdf).
const shardVolumeDevice = "/dev/sdf"

// freshShardVolumeGB is the size of a newly-created shard data volume.
const freshShardVolumeGB int32 = 50

// ShardLaunchAPI is the launch primitive the shard FSM depends on. Narrow
// so test fakes don't have to stand up a whole EC2 client.
type ShardLaunchAPI interface {
	Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error)
	Terminate(ctx context.Context, instanceID string) error
}

// LaunchSpec describes one shard instance to bring up. ExistingVolumeID
// distinguishes a fresh launch (empty: create a new gp3 volume) from a
// recovery launch (set: reattach the surviving volume). The (Tier, AZ)
// pair must resolve to a configured subnet + the tier to a launch template,
// or Launch errors fast — silently falling back would hide config drift.
type LaunchSpec struct {
	ShardID          string
	Tier             string
	AZ               string
	ExistingVolumeID string // "" → fresh (create volume); set → recovery (reattach)
	SizeGB           int32  // fresh-launch volume size; ignored on recovery
}

// LaunchResult carries the identifiers the shard SM persists onto its entry
// once a launch completes.
type LaunchResult struct {
	InstanceID string
	VolumeID   string
	AZ         string
}

// EC2RunAPI is the slice of the EC2 client EC2Provisioner needs. RunInstances
// stamps the shard_id tag at birth (via TagSpecifications), so there's no
// post-launch CreateTags; DescribeInstances backs both the idempotency
// find-by-tag and the wait-for-running poll.
type EC2RunAPI interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// EC2Provisioner launches and terminates shard instances directly via the
// EC2 API — no Auto Scaling Group. The coordinator is the sole capacity
// authority: every instance exists because EC2Provisioner ran it, and is
// reaped because EC2Provisioner terminated it. Because nothing else can
// create a shard instance, there is no "ghost" class — the failure mode of
// the ASG model, where the group's own capacity maintenance launched bare
// instances the coordinator neither tagged, claimed, nor reaped.
//
// Launch is idempotent: it first looks for a pending|running instance
// already tagged with the shard id and adopts it, so a crash-replay or
// retry reuses the in-flight instance instead of minting a twin. A
// terminated instance is excluded by the state filter, so a recovery
// relaunch (after the dead instance is reaped) correctly starts fresh.
type EC2Provisioner struct {
	ec2     EC2RunAPI
	volumes VolumeManager

	// subnets[tier][az] = subnet id; templates[tier] = launch template id.
	// Both immutable after construction — the provisioner only ever picks
	// from these sets.
	subnets   map[string]map[string]string
	templates map[string]string

	pollInterval time.Duration
	pollAttempts int
}

var (
	_ ShardLaunchAPI = (*EC2Provisioner)(nil)
	_ ShardLauncher  = (*EC2Provisioner)(nil)
)

// NewEC2Provisioner builds a real EC2-backed provisioner. subnets maps
// tier → az → subnet id; templates maps tier → launch template id (both
// parsed from SHARD_<TIER>_SUBNETS / SHARD_<TIER>_LAUNCH_TEMPLATE at boot).
func NewEC2Provisioner(region string, subnets map[string]map[string]string, templates map[string]string, volumes VolumeManager) (*EC2Provisioner, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return NewEC2ProvisionerWithClient(ec2.NewFromConfig(cfg), volumes, subnets, templates), nil
}

// NewEC2ProvisionerWithClient injects collaborators (for tests).
func NewEC2ProvisionerWithClient(client EC2RunAPI, volumes VolumeManager, subnets map[string]map[string]string, templates map[string]string) *EC2Provisioner {
	return &EC2Provisioner{
		ec2:          client,
		volumes:      volumes,
		subnets:      subnets,
		templates:    templates,
		pollInterval: 5 * time.Second,
		pollAttempts: 36, // ~3 min ceiling on reaching `running`
	}
}

// AZsForTier returns the tier's configured AZs, sorted so stripe-index
// assignment is stable across calls and restarts. Nil for an unknown tier.
func (p *EC2Provisioner) AZsForTier(tier string) []string {
	azMap, ok := p.subnets[tier]
	if !ok {
		return nil
	}
	azs := make([]string, 0, len(azMap))
	for az := range azMap {
		azs = append(azs, az)
	}
	sort.Strings(azs)
	return azs
}

// StripeAZ maps a stripe index to an AZ for the tier: shard at index i lands
// in AZ i % len(azs). Pure, given the configured (immutable) AZ set. Returns
// "" if the tier has no configured AZs.
func (p *EC2Provisioner) StripeAZ(tier string, index int) string {
	azs := p.AZsForTier(tier)
	if len(azs) == 0 {
		return ""
	}
	if index < 0 {
		index = -index
	}
	return azs[index%len(azs)]
}

// Launch brings up one shard instance for spec and returns its identifiers
// once the instance is running and its volume attached. Fresh vs recovery is
// decided by spec.ExistingVolumeID. Idempotent: an instance already tagged
// for this shard (pending|running) is adopted rather than duplicated.
func (p *EC2Provisioner) Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error) {
	subnet, ok := p.subnets[spec.Tier][spec.AZ]
	if !ok {
		return LaunchResult{}, fmt.Errorf("launch %s: no subnet configured for tier=%q az=%q (tier has: %v)",
			spec.ShardID, spec.Tier, spec.AZ, p.AZsForTier(spec.Tier))
	}
	templateID, ok := p.templates[spec.Tier]
	if !ok {
		return LaunchResult{}, fmt.Errorf("launch %s: no launch template configured for tier=%q", spec.ShardID, spec.Tier)
	}

	// Idempotency: adopt an in-flight instance already tagged for this shard
	// rather than minting a twin (crash-replay / retry safety). The
	// state filter excludes a just-reaped instance, so a recovery relaunch
	// still starts fresh.
	instanceID, err := p.findLiveByShard(ctx, spec.ShardID)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: %w", spec.ShardID, err)
	}
	if instanceID != "" {
		log.Printf("ec2-provisioner: adopting in-flight instance %s already tagged for %s (idempotent relaunch)", instanceID, spec.ShardID)
	} else {
		instanceID, err = p.runInstance(ctx, spec, subnet, templateID)
		if err != nil {
			return LaunchResult{}, fmt.Errorf("launch %s: %w", spec.ShardID, err)
		}
	}

	// AttachVolume requires the instance to be `running`; wait for it. This
	// is a real instance-state wait (seconds), not the ASG model's
	// poll-the-group-until-InService discovery dance.
	if err := p.waitForRunning(ctx, instanceID); err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: %w", spec.ShardID, err)
	}

	volumeID := spec.ExistingVolumeID
	if volumeID == "" {
		// Idempotency for the volume too: a crash-replay of a fresh launch
		// (instance adopted above) must reattach the volume it already
		// created rather than mint a second, orphaned one. The same tag
		// lookup recovery already relies on.
		if info, ferr := p.volumes.FindByShard(ctx, spec.ShardID); ferr != nil {
			return LaunchResult{}, fmt.Errorf("launch %s: find volume: %w", spec.ShardID, ferr)
		} else if info != nil {
			volumeID = info.VolumeID
		}
	}
	if volumeID == "" {
		volumeID, err = p.volumes.CreateForShard(ctx, CreateVolumeOpts{
			ShardID: spec.ShardID,
			Tier:    spec.Tier,
			AZ:      spec.AZ,
			SizeGB:  spec.SizeGB,
		})
		if err != nil {
			return LaunchResult{}, fmt.Errorf("launch %s: create volume: %w", spec.ShardID, err)
		}
	}
	if err := p.volumes.AttachToInstance(ctx, volumeID, instanceID, shardVolumeDevice); err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: attach %s→%s: %w", spec.ShardID, volumeID, instanceID, err)
	}

	log.Printf("ec2-provisioner: launched %s (instance=%s vol=%s az=%s subnet=%s)",
		spec.ShardID, instanceID, volumeID, spec.AZ, subnet)
	return LaunchResult{InstanceID: instanceID, VolumeID: volumeID, AZ: spec.AZ}, nil
}

// Terminate removes a shard instance. An already-gone instance is success
// (the benign race where it died between our decision and the call).
func (p *EC2Provisioner) Terminate(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("Terminate: instance id required")
	}
	_, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil && instanceAlreadyGone(err) {
		log.Printf("ec2-provisioner: instance %s already gone, ignoring: %v", instanceID, err)
		return nil
	}
	return err
}

// findLiveByShard returns the id of a pending|running instance tagged
// shard_id=<id>, or "" if none. The state filter is deliberate: a shard's
// just-reaped (shutting-down/terminated) instance must NOT be adopted by the
// recovery relaunch, only a genuinely in-flight one (crash-replay).
func (p *EC2Provisioner) findLiveByShard(ctx context.Context, shardID string) (string, error) {
	out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:shard_id"), Values: []string{shardID}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("find instance by shard_id=%s: %w", shardID, err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceId != nil {
				return *inst.InstanceId, nil
			}
		}
	}
	return "", nil
}

// runInstance launches exactly one instance from the tier's launch template
// into the AZ's subnet, tagging it shard_id at birth so it is never an
// untagged "ghost", not even momentarily. No ClientToken: idempotency comes
// from findLiveByShard (a stable token would, across recovery generations,
// return the prior — terminated — instance instead of launching a new one).
func (p *EC2Provisioner) runInstance(ctx context.Context, spec LaunchSpec, subnet, templateID string) (string, error) {
	out, err := p.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(templateID),
			Version:          aws.String("$Latest"),
		},
		SubnetId: aws.String(subnet),
		MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags: []ec2types.Tag{
				{Key: aws.String("shard_id"), Value: aws.String(spec.ShardID)},
				{Key: aws.String("Name"), Value: aws.String(spec.ShardID)},
			},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("RunInstances(template=%s subnet=%s): %w", templateID, subnet, err)
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		return "", fmt.Errorf("RunInstances(template=%s subnet=%s): no instance returned", templateID, subnet)
	}
	return *out.Instances[0].InstanceId, nil
}

// waitForRunning polls the instance until it reports `running`. Errors if it
// reaches a terminal/stopping state first (a launch that died on boot) or the
// poll budget is spent.
func (p *EC2Provisioner) waitForRunning(ctx context.Context, instanceID string) error {
	for attempt := 0; attempt < p.pollAttempts; attempt++ {
		out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return fmt.Errorf("describe %s: %w", instanceID, err)
		}
		state := instanceState(out, instanceID)
		switch state {
		case "running":
			return nil
		case "shutting-down", "terminated", "stopping", "stopped":
			return fmt.Errorf("instance %s reached terminal state %q before running", instanceID, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.pollInterval):
		}
	}
	return fmt.Errorf("instance %s not running after %d polls", instanceID, p.pollAttempts)
}

// instanceState extracts the named instance's state-name from a
// DescribeInstances response, or "" if absent.
func instanceState(out *ec2.DescribeInstancesOutput, instanceID string) string {
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceId != nil && *inst.InstanceId == instanceID && inst.State != nil {
				return string(inst.State.Name)
			}
		}
	}
	return ""
}

// LaunchInstance / TerminateInstance satisfy the legacy ShardLauncher
// interface so NewProvisioner accepts an *EC2Provisioner. The FSM drives
// launches through the ShardLaunchAPI surface (Launch/Terminate); these exist
// only for the constructor's parameter type.
func (p *EC2Provisioner) LaunchInstance(context.Context, string) (string, error) {
	return "", fmt.Errorf("EC2Provisioner.LaunchInstance is a no-op; launches go through Launch(spec)")
}

// TerminateInstance delegates to Terminate using the instance id (addr
// ignored — instances are tracked by id, read from shard_records).
func (p *EC2Provisioner) TerminateInstance(ctx context.Context, id, _ string) error {
	return p.Terminate(ctx, id)
}

// instanceAlreadyGone reports whether a terminate error means the instance is
// already absent (a benign race we treat as success).
func instanceAlreadyGone(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no longer exists") ||
		strings.Contains(msg, "InvalidInstanceID")
}
