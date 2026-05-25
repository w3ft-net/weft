package logdb

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ASGTaggerAPI is the slice of the EC2 client ASGProvisioner needs to
// stamp a shard_id tag onto a freshly-launched instance. Volume
// tagging lives on the VolumeManager; this is instance-only, so it
// stays a one-method interface rather than reusing EC2VolumesAPI.
type ASGTaggerAPI interface {
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

// shardVolumeDevice is the block device the shard binary mounts for
// its DuckDB data (matches entrypoint.sh's mount of /dev/sdf).
const shardVolumeDevice = "/dev/sdf"

// LaunchSpec describes one shard instance to bring up. ExistingVolumeID
// distinguishes a fresh launch (empty: create a new gp3 volume) from a
// recovery launch (set: reattach the surviving volume). The (Tier, AZ)
// pair must be present in the provisioner's configured ASG map, or
// Launch errors fast — silently falling back to another AZ would hide
// TF config drift and rebalance the stripe behind our back.
type LaunchSpec struct {
	ShardID          string
	Tier             string
	AZ               string
	ExistingVolumeID string // "" → fresh (create volume); set → recovery (reattach)
	SizeGB           int32  // fresh-launch volume size; ignored on recovery
}

// LaunchResult carries the identifiers the shard SM persists onto its
// entry once a launch completes.
type LaunchResult struct {
	InstanceID string
	VolumeID   string
	AZ         string
}

// ASGProvisioner launches and terminates shard instances through
// per-AZ Auto Scaling Groups. Each (tier, AZ) maps to exactly one ASG
// (min=0, no scaling policies, no lifecycle hooks, EC2 health checks
// off); the provisioner is the sole authority that moves their
// desired_capacity. A launch bumps desired by one, polls the group for
// the instance that appears, tags it with the shard id, and attaches
// the shard's EBS volume.
//
// Concurrency: a per-(tier, AZ) mutex serialises launches into the
// same ASG, so two concurrent launches can't both read desired=N and
// both set N+1 (which yields one instance, not two).
type ASGProvisioner struct {
	asg     ASGApi
	ec2     ASGTaggerAPI
	volumes VolumeManager

	// asgs[tier][az] = ASG name. Immutable after construction — the
	// provisioner only ever picks from this set, never mints ASGs.
	asgs map[string]map[string]string

	pollInterval time.Duration
	pollAttempts int

	mu          sync.Mutex // guards launchLocks
	launchLocks map[string]*sync.Mutex
}

// NewASGProvisioner builds a real ASG-backed provisioner. asgs maps
// tier → az → ASG name (parsed from SHARD_<TIER>_ASGS at boot).
func NewASGProvisioner(region string, asgs map[string]map[string]string, volumes VolumeManager) (*ASGProvisioner, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	ec2Client := ec2.NewFromConfig(cfg)
	return NewASGProvisionerWithClients(autoscaling.NewFromConfig(cfg), ec2Client, volumes, asgs), nil
}

// NewASGProvisionerWithClients injects collaborators (for tests).
func NewASGProvisionerWithClients(asg ASGApi, tagger ASGTaggerAPI, volumes VolumeManager, asgs map[string]map[string]string) *ASGProvisioner {
	return &ASGProvisioner{
		asg:          asg,
		ec2:          tagger,
		volumes:      volumes,
		asgs:         asgs,
		pollInterval: 5 * time.Second,
		pollAttempts: 36, // ~3 min ceiling on a launch
		launchLocks:  map[string]*sync.Mutex{},
	}
}

// AZsForTier returns the tier's configured AZs, sorted so stripe-index
// assignment is stable across calls and restarts. Nil for an unknown
// tier.
func (p *ASGProvisioner) AZsForTier(tier string) []string {
	azMap, ok := p.asgs[tier]
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

// StripeAZ maps a stripe index to an AZ for the tier: shard at index i
// lands in AZ i % len(azs). Pure, given the configured (immutable) AZ
// set. Returns "" if the tier has no configured AZs.
func (p *ASGProvisioner) StripeAZ(tier string, index int) string {
	azs := p.AZsForTier(tier)
	if len(azs) == 0 {
		return ""
	}
	if index < 0 {
		index = -index
	}
	return azs[index%len(azs)]
}

// Launch brings up one shard instance for spec and returns its
// identifiers once the instance is InService and its volume attached.
// Fresh vs recovery is decided by spec.ExistingVolumeID. The whole
// sequence holds the per-(tier, AZ) lock so concurrent launches into
// the same ASG serialise around the read-desired / set-desired+1 step.
func (p *ASGProvisioner) Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error) {
	asgName, ok := p.asgs[spec.Tier][spec.AZ]
	if !ok {
		return LaunchResult{}, fmt.Errorf("launch %s: no ASG configured for tier=%q az=%q (tier has: %v)",
			spec.ShardID, spec.Tier, spec.AZ, p.AZsForTier(spec.Tier))
	}

	lock := p.lockFor(spec.Tier, spec.AZ)
	lock.Lock()
	defer lock.Unlock()

	before, desired, err := p.groupSnapshot(ctx, asgName)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: %w", spec.ShardID, err)
	}

	want := desired + 1
	if _, err := p.asg.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: &asgName,
		DesiredCapacity:      &want,
	}); err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: SetDesiredCapacity(%s=%d): %w", spec.ShardID, asgName, want, err)
	}

	instanceID, err := p.awaitNewInstance(ctx, asgName, before)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: %w", spec.ShardID, err)
	}

	if _, err := p.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags:      []ec2types.Tag{{Key: aws.String("shard_id"), Value: aws.String(spec.ShardID)}},
	}); err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: tag instance %s: %w", spec.ShardID, instanceID, err)
	}

	volumeID := spec.ExistingVolumeID
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

	log.Printf("asg-provisioner: launched %s on %s (instance=%s vol=%s az=%s)",
		spec.ShardID, asgName, instanceID, volumeID, spec.AZ)
	return LaunchResult{InstanceID: instanceID, VolumeID: volumeID, AZ: spec.AZ}, nil
}

// Terminate removes a shard instance and decrements the ASG's desired
// capacity — the archive-then-reclaim path, where the bucket is not
// expecting a replacement. An already-gone instance is success. (The
// recovery path, where capacity must stay constant so the ASG relaunches,
// is the shard SM's responsibility before it calls Launch.)
func (p *ASGProvisioner) Terminate(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("Terminate: instance id required")
	}
	decrement := true
	_, err := p.asg.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     &instanceID,
		ShouldDecrementDesiredCapacity: &decrement,
	})
	if err != nil && instanceAlreadyGone(err) {
		log.Printf("asg-provisioner: instance %s already gone, ignoring: %v", instanceID, err)
		return nil
	}
	return err
}

// groupSnapshot returns the set of InstanceIDs currently in the ASG and
// its desired capacity. The set is the "before" baseline a launch
// diffs against to identify the instance it caused.
func (p *ASGProvisioner) groupSnapshot(ctx context.Context, asgName string) (map[string]bool, int32, error) {
	g, err := p.describeOne(ctx, asgName)
	if err != nil {
		return nil, 0, err
	}
	set := make(map[string]bool, len(g.Instances))
	for _, inst := range g.Instances {
		if inst.InstanceId != nil {
			set[*inst.InstanceId] = true
		}
	}
	var desired int32
	if g.DesiredCapacity != nil {
		desired = *g.DesiredCapacity
	}
	return set, desired, nil
}

// awaitNewInstance polls the ASG until an instance absent from before
// reaches InService, returning its id. Times out after
// pollInterval × pollAttempts, or when ctx is cancelled.
func (p *ASGProvisioner) awaitNewInstance(ctx context.Context, asgName string, before map[string]bool) (string, error) {
	for attempt := 0; attempt < p.pollAttempts; attempt++ {
		g, err := p.describeOne(ctx, asgName)
		if err != nil {
			return "", err
		}
		for _, inst := range g.Instances {
			if inst.InstanceId == nil || before[*inst.InstanceId] {
				continue
			}
			if inst.LifecycleState == astypes.LifecycleStateInService {
				return *inst.InstanceId, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(p.pollInterval):
		}
	}
	return "", fmt.Errorf("no new InService instance in %s after %d polls", asgName, p.pollAttempts)
}

// describeOne fetches a single ASG, erroring if it isn't found.
func (p *ASGProvisioner) describeOne(ctx context.Context, asgName string) (astypes.AutoScalingGroup, error) {
	out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return astypes.AutoScalingGroup{}, fmt.Errorf("describe ASG %s: %w", asgName, err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return astypes.AutoScalingGroup{}, fmt.Errorf("ASG %s not found", asgName)
	}
	return out.AutoScalingGroups[0], nil
}

// lockFor returns the launch mutex for a (tier, AZ), creating it on
// first use.
func (p *ASGProvisioner) lockFor(tier, az string) *sync.Mutex {
	key := tier + "/" + az
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.launchLocks[key]
	if !ok {
		m = &sync.Mutex{}
		p.launchLocks[key] = m
	}
	return m
}

// instanceAlreadyGone reports whether an ASG-terminate error means the
// instance is already absent (a benign race we treat as success).
func instanceAlreadyGone(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no longer exists") ||
		strings.Contains(msg, "InvalidInstanceID")
}
