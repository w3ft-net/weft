package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// shardGitSHATag is the AMI tag carrying the shard-subtree git SHA the AMI was
// built from. The build stamps it (packer/weft.pkr.hcl); bump-shard-templates
// points the launch templates at such an AMI, and roll-all reads it back to
// decide which live shards are stale (their reported binary_sha differs).
const shardGitSHATag = "ShardGitSHA"

// shardTemplateEnvByTier maps a shard tier to the env var carrying its launch
// template id — the same vars the coordinator reads at boot
// (logdb.parseShardTemplates). Kept local rather than importing logdb so
// weft-admin stays a lightweight, CGO-free coordinator-resident binary.
var shardTemplateEnvByTier = map[string]string{
	"small":  "SHARD_SMALL_LAUNCH_TEMPLATE",
	"medium": "SHARD_MEDIUM_LAUNCH_TEMPLATE",
	"large":  "SHARD_LARGE_LAUNCH_TEMPLATE",
}

// awsRegion resolves the region the way the coordinator processes do: AWS_REGION
// (set by user-data on EC2) with a us-east-2 fallback. The SDK default chain
// doesn't reliably resolve region from IMDS, so we pass it explicitly.
func awsRegion() string {
	if r := strings.TrimSpace(os.Getenv("AWS_REGION")); r != "" {
		return r
	}
	return "us-east-2"
}

// appName resolves APP_NAME (set on the coordinator), defaulting to weft. Used
// to build the /<app>/ami/latest SSM parameter path.
func appName() string {
	if a := strings.TrimSpace(os.Getenv("APP_NAME")); a != "" {
		return a
	}
	return "weft"
}

// loadAWSConfig loads the default AWS config with the resolved region. On the
// coordinator this picks up the instance role via IMDS.
func loadAWSConfig(ctx context.Context) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(awsRegion()))
}

// shardLaunchTemplateIDs returns tier→launch-template-id for the tiers that set
// SHARD_<TIER>_LAUNCH_TEMPLATE. Empty map if none are configured.
func shardLaunchTemplateIDs() map[string]string {
	out := map[string]string{}
	for tier, env := range shardTemplateEnvByTier {
		if id := strings.TrimSpace(os.Getenv(env)); id != "" {
			out[tier] = id
		}
	}
	return out
}

// sortedTiers returns the tiers of templates in deterministic order.
func sortedTiers(templates map[string]string) []string {
	tiers := make([]string, 0, len(templates))
	for t := range templates {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	return tiers
}

// resolveShardTargetSHA loads AWS config and resolves the shard SHA the fleet
// should be on from the coordinator's *own* AMI tag. Convenience wrapper for
// callers (roll-all) that don't otherwise hold an EC2 client.
func resolveShardTargetSHA(ctx context.Context) (string, error) {
	amiID, err := ownImageID(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve own AMI from IMDS: %w", err)
	}
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return "", err
	}
	return shardTargetSHAFromAMI(ctx, ec2.NewFromConfig(cfg), amiID)
}

// ownImageID reads the running instance's AMI id from IMDS
// (/latest/meta-data/ami-id). weft-admin runs on the coordinator, which shares a
// single weft AMI with the shards — so its own image is the source of truth for
// "what SHA the fleet should be on", and it's guaranteed live (the box booted
// from it), unlike the launch template's possibly stale/deleted AMI.
func ownImageID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := imds.New(imds.Options{}).GetMetadata(ctx, &imds.GetMetadataInput{Path: "ami-id"})
	if err != nil {
		return "", err
	}
	defer out.Content.Close()
	b, err := io.ReadAll(out.Content)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("IMDS returned empty ami-id")
	}
	return id, nil
}

// shardTargetSHAFromAMI follows an AMI id to its ShardGitSHA tag — the shard
// SHA the fleet *should* be on.
//
// Returns "" (meaning "roll everything", per shardsToRoll's empty-target
// semantics) when the AMI carries no ShardGitSHA tag — the bootstrap case for
// AMIs built before tagging existed.
func shardTargetSHAFromAMI(ctx context.Context, ec2c *ec2.Client, amiID string) (string, error) {
	imgs, err := ec2c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{amiID}})
	if err != nil {
		return "", fmt.Errorf("describe image %s: %w", amiID, err)
	}
	if len(imgs.Images) == 0 {
		return "", fmt.Errorf("image %s not found", amiID)
	}
	for _, tag := range imgs.Images[0].Tags {
		if aws.ToString(tag.Key) == shardGitSHATag {
			return aws.ToString(tag.Value), nil
		}
	}
	return "", nil // untagged AMI → roll everything
}
