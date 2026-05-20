package logdb

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// ASGApi is the subset of the autoscaling client we use.
type ASGApi interface {
	SetDesiredCapacity(ctx context.Context, params *autoscaling.SetDesiredCapacityInput, optFns ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error)
	DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	TerminateInstanceInAutoScalingGroup(ctx context.Context, params *autoscaling.TerminateInstanceInAutoScalingGroupInput, optFns ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
}

// EC2Api is the subset of the ec2 client we use. Kept on the
// constructor signature so callers can continue to inject a
// real ec2 client; nothing in ASGLauncher consumes it today
// (instance-level discovery happens via the lifecycle hook + the
// LAUNCHING-row handoff).
type EC2Api interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// ASGLauncher implements ShardLauncher + TierScaler. Under §3 the
// coordinator sets a tier's ASG desired_capacity absolutely from
// bucket config; ASGLauncher's job is the thin wrapper around
// SetDesiredCapacity / DescribeAutoScalingGroups /
// TerminateInstanceInAutoScalingGroup.
type ASGLauncher struct {
	asgClient ASGApi
	tiers     map[string]string // tier → ASG name
}

// UsesLaunchingRows reports that ASGLauncher is an async launcher —
// the reconciler INSERTs a LAUNCHING row before scaling, the
// lifecycle hook claims it when AWS brings the instance up.
func (l *ASGLauncher) UsesLaunchingRows() bool { return true }

func NewASGLauncher(region string, tiers map[string]string, _ int) (*ASGLauncher, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &ASGLauncher{
		asgClient: autoscaling.NewFromConfig(cfg),
		tiers:     tiers,
	}, nil
}

// NewASGLauncherWithClients creates a launcher with injected clients
// (for testing). The EC2 client parameter is retained for call-site
// stability but unused.
func NewASGLauncherWithClients(asg ASGApi, _ EC2Api, tiers map[string]string, _ int) *ASGLauncher {
	return &ASGLauncher{
		asgClient: asg,
		tiers:     tiers,
	}
}

// LaunchInstance is stale under §3. The reconciler mints a LAUNCHING
// row, the tier's desired_capacity gets set absolutely at the end
// of each tick via syncASGDesiredCapacity, and the lifecycle hook
// promotes via heartbeat. Callers that still invoke LaunchInstance
// on the ASG path are a bug; returning an error surfaces it.
func (l *ASGLauncher) LaunchInstance(_ context.Context, tier string) (string, error) {
	return "", fmt.Errorf("ASGLauncher.LaunchInstance is a no-op for tier %q; async scaling goes through syncASGDesiredCapacity", tier)
}

// SetDesiredCapacity sets a tier's ASG desired_capacity absolutely.
// Called once per tick per tier by the provisioner's
// syncASGDesiredCapacity pass.
func (l *ASGLauncher) SetDesiredCapacity(ctx context.Context, tier string, desired int32) error {
	asgName, ok := l.tiers[tier]
	if !ok {
		return fmt.Errorf("unknown tier %q (available: %v)", tier, l.tierNames())
	}
	_, err := l.asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: &asgName,
		DesiredCapacity:      &desired,
	})
	if err != nil {
		return fmt.Errorf("SetDesiredCapacity(%s=%d): %w", asgName, desired, err)
	}
	return nil
}

// TierMaxSize returns the ASG's MaxSize — the static upper bound TF
// sets. The provisioner clamps computed desired to this and logs a
// tripwire when the clamp kicks in.
func (l *ASGLauncher) TierMaxSize(ctx context.Context, tier string) (int32, error) {
	asgName, ok := l.tiers[tier]
	if !ok {
		return 0, fmt.Errorf("unknown tier %q", tier)
	}
	out, err := l.asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return 0, fmt.Errorf("describe ASG %s: %w", asgName, err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return 0, fmt.Errorf("ASG %s not found", asgName)
	}
	g := out.AutoScalingGroups[0]
	if g.MaxSize == nil {
		return 0, nil
	}
	return *g.MaxSize, nil
}

// TerminateForReplacement asks the ASG to terminate a dead instance
// WITHOUT decrementing desired_capacity — the ASG then launches a
// replacement, which picks up the shard's EBS volume via the
// lifecycle hook's RECOVER path. Distinct from TerminateInstance
// (which decrements) because the semantics are opposite: here we
// want ASG to keep capacity constant and swap instances; there we
// want ASG to shrink.
func (l *ASGLauncher) TerminateForReplacement(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("TerminateForReplacement: instance id required")
	}
	shouldDecrement := false
	log.Printf("asg-launcher: requesting replacement of dead instance %s", instanceID)
	_, err := l.asgClient.TerminateInstanceInAutoScalingGroup(ctx,
		&autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     &instanceID,
			ShouldDecrementDesiredCapacity: &shouldDecrement,
		})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") ||
			strings.Contains(msg, "no longer exists") ||
			strings.Contains(msg, "InvalidInstanceID") {
			log.Printf("asg-launcher: instance %s already gone, ignoring: %v", instanceID, err)
			return nil
		}
	}
	return err
}

// TerminateInstance terminates a specific EC2 instance in its ASG
// and decrements desired_capacity. The id must be provided — post-§3
// there's no addr→id tracking; callers read InstanceID from
// shard_records.
func (l *ASGLauncher) TerminateInstance(ctx context.Context, id, _ string) error {
	if id == "" {
		return fmt.Errorf("TerminateInstance: instance id required")
	}
	shouldDecrement := true
	log.Printf("asg-launcher: terminating instance %s", id)
	_, err := l.asgClient.TerminateInstanceInAutoScalingGroup(ctx,
		&autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     &id,
			ShouldDecrementDesiredCapacity: &shouldDecrement,
		})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") ||
			strings.Contains(msg, "no longer exists") ||
			strings.Contains(msg, "InvalidInstanceID") {
			log.Printf("asg-launcher: instance %s already gone, ignoring: %v", id, err)
			return nil
		}
	}
	return err
}

func (l *ASGLauncher) tierNames() []string {
	var names []string
	for k := range l.tiers {
		names = append(names, k)
	}
	return names
}

// strPtr is a tiny helper for string-pointer literals that the AWS
// SDK is heavy on. Used from tests that mock EC2 responses.
func strPtr(s string) *string { return &s }
