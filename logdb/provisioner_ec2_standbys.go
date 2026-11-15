package logdb

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Warm-pool tags. shard_spare marks a pool instance from birth until it
// becomes a shard's (ReleaseStandby); the shard binary and startup.sh read
// it through IMDS to boot in standby mode (no volume, no database). A claim
// adds shard_id, which is also what lets a recovery launch adopt a standby
// it claimed before a crash (findLiveByShard).
const (
	tagStandby     = "shard_spare"
	tagStandbyTier = "shard_tier"
	tagStandbyAZ   = "shard_az"
)

var _ StandbyLauncher = (*EC2Provisioner)(nil)

// CurrentImageID is the AMI shards launch from: the coordinator's own
// (empty when IMDS couldn't say, and then launches use the template's).
func (p *EC2Provisioner) CurrentImageID() string { return p.imageID }

// LaunchStandby runs one standby from the tier's launch template into the AZ's
// subnet, tagged as a standby (and as claimed by claimFor, when set) at
// birth. It does not wait for running: the standby's first heartbeat is
// what makes it useful, and the pool tracks that.
func (p *EC2Provisioner) LaunchStandby(ctx context.Context, tier, az, claimFor string) (StandbyInstance, error) {
	subnet, ok := p.subnets[tier][az]
	if !ok {
		return StandbyInstance{}, fmt.Errorf("launch spare: no subnet configured for tier=%q az=%q (tier has: %v)", tier, az, p.AZsForTier(tier))
	}
	templateID, ok := p.templates[tier]
	if !ok {
		return StandbyInstance{}, fmt.Errorf("launch spare: no launch template configured for tier=%q", tier)
	}
	tags := []ec2types.Tag{
		{Key: aws.String(tagStandby), Value: aws.String("true")},
		{Key: aws.String(tagStandbyTier), Value: aws.String(tier)},
		{Key: aws.String(tagStandbyAZ), Value: aws.String(az)},
		{Key: aws.String("Name"), Value: aws.String("spare-" + tier + "-" + az)},
	}
	if claimFor != "" {
		tags = append(tags, ec2types.Tag{Key: aws.String("shard_id"), Value: aws.String(claimFor)})
	}
	in := &ec2.RunInstancesInput{
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(templateID),
			Version:          aws.String("$Latest"),
		},
		SubnetId:          aws.String(subnet),
		MinCount:          aws.Int32(1),
		MaxCount:          aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}},
	}
	if p.imageID != "" {
		in.ImageId = aws.String(p.imageID)
	}
	out, err := p.ec2.RunInstances(ctx, in)
	if err != nil {
		return StandbyInstance{}, fmt.Errorf("RunInstances spare (template=%s subnet=%s image=%s): %w", templateID, subnet, p.imageID, err)
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		return StandbyInstance{}, fmt.Errorf("RunInstances spare (template=%s subnet=%s): no instance returned", templateID, subnet)
	}
	return StandbyInstance{
		InstanceID: *out.Instances[0].InstanceId,
		Tier:       tier,
		AZ:         az,
		ImageID:    p.imageID,
		ClaimedBy:  claimFor,
	}, nil
}

// ListStandbyInstances returns every pending or running instance still
// tagged as a standby.
func (p *EC2Provisioner) ListStandbyInstances(ctx context.Context) ([]StandbyInstance, error) {
	in := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag-key"), Values: []string{tagStandby}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	}
	var out []StandbyInstance
	for {
		resp, err := p.ec2.DescribeInstances(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("describe spare instances: %w", err)
		}
		for _, r := range resp.Reservations {
			for _, inst := range r.Instances {
				sp := StandbyInstance{InstanceID: aws.ToString(inst.InstanceId), ImageID: aws.ToString(inst.ImageId)}
				if inst.LaunchTime != nil {
					sp.LaunchedAt = *inst.LaunchTime
				}
				for _, t := range inst.Tags {
					switch aws.ToString(t.Key) {
					case tagStandbyTier:
						sp.Tier = aws.ToString(t.Value)
					case tagStandbyAZ:
						sp.AZ = aws.ToString(t.Value)
					case "shard_id":
						sp.ClaimedBy = aws.ToString(t.Value)
					}
				}
				out = append(out, sp)
			}
		}
		if resp.NextToken == nil {
			return out, nil
		}
		in.NextToken = resp.NextToken
	}
}

// ClaimInstance stamps the claiming shard on a standby.
func (p *EC2Provisioner) ClaimInstance(ctx context.Context, instanceID, shardID string) error {
	_, err := p.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String("shard_id"), Value: aws.String(shardID)},
			{Key: aws.String("Name"), Value: aws.String(shardID)},
		},
	})
	if err != nil {
		return fmt.Errorf("tag %s claimed by %s: %w", instanceID, shardID, err)
	}
	return nil
}

// ReleaseStandby drops the standby tag from an instance that is now a shard's,
// so it is no longer listed as a standby and a reboot comes up as the shard.
func (p *EC2Provisioner) ReleaseStandby(ctx context.Context, instanceID string) error {
	_, err := p.ec2.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{instanceID},
		Tags:      []ec2types.Tag{{Key: aws.String(tagStandby)}},
	})
	if err != nil {
		return fmt.Errorf("untag spare %s: %w", instanceID, err)
	}
	return nil
}
