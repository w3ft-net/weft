package coordinator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ASGApi is the subset of the autoscaling client we use.
type ASGApi interface {
	SetDesiredCapacity(ctx context.Context, params *autoscaling.SetDesiredCapacityInput, optFns ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error)
	DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	TerminateInstanceInAutoScalingGroup(ctx context.Context, params *autoscaling.TerminateInstanceInAutoScalingGroupInput, optFns ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
}

// EC2Api is the subset of the ec2 client we use.
type EC2Api interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// ASGLauncher implements ShardLauncher by scaling AWS Auto Scaling Groups.
type ASGLauncher struct {
	asgClient ASGApi
	ec2Client EC2Api
	tiers     map[string]string // tier → ASG name
	port      int
	mu        sync.Mutex
	instances map[string]string // addr → instance ID
	known     map[string]bool   // instance IDs already claimed

	// PollInterval and PollAttempts control how long we wait for new instances.
	// Exported for testing. Defaults: 5s interval, 60 attempts (5 min).
	PollInterval time.Duration
	PollAttempts int
}

func NewASGLauncher(region string, tiers map[string]string, port int) (*ASGLauncher, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &ASGLauncher{
		asgClient:    autoscaling.NewFromConfig(cfg),
		ec2Client:    ec2.NewFromConfig(cfg),
		tiers:        tiers,
		port:         port,
		instances:    make(map[string]string),
		known:        make(map[string]bool),
		PollInterval: 5 * time.Second,
		PollAttempts: 60,
	}, nil
}

// NewASGLauncherWithClients creates a launcher with injected clients (for testing).
func NewASGLauncherWithClients(asg ASGApi, ec2c EC2Api, tiers map[string]string, port int) *ASGLauncher {
	return &ASGLauncher{
		asgClient:    asg,
		ec2Client:    ec2c,
		tiers:        tiers,
		port:         port,
		instances:    make(map[string]string),
		known:        make(map[string]bool),
		PollInterval: 10 * time.Millisecond, // fast for tests
		PollAttempts: 20,
	}
}

func (l *ASGLauncher) LaunchInstance(ctx context.Context, tier string) (string, error) {
	asgName, ok := l.tiers[tier]
	if !ok {
		return "", fmt.Errorf("unknown tier %q (available: %v)", tier, l.tierNames())
	}

	// Snapshot current instance IDs before scaling up.
	l.mu.Lock()
	beforeIDs := make(map[string]bool)
	for id := range l.known {
		beforeIDs[id] = true
	}
	l.mu.Unlock()

	currentIDs, err := l.asgInstanceIDs(ctx, asgName)
	if err != nil {
		return "", fmt.Errorf("describe ASG %s: %w", asgName, err)
	}
	for _, id := range currentIDs {
		beforeIDs[id] = true
	}

	// Increase desired capacity.
	desired := int32(len(currentIDs) + 1)
	log.Printf("asg-launcher: scaling %s to %d instances", asgName, desired)
	_, err = l.asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: &asgName,
		DesiredCapacity:      &desired,
	})
	if err != nil {
		return "", fmt.Errorf("SetDesiredCapacity: %w", err)
	}

	// Poll for a new InService instance.
	var newInstanceID string
	for i := 0; i < l.PollAttempts; i++ {
		time.Sleep(l.PollInterval)
		ids, err := l.asgInstanceIDs(ctx, asgName)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if !beforeIDs[id] {
				newInstanceID = id
				break
			}
		}
		if newInstanceID != "" {
			break
		}
	}
	if newInstanceID == "" {
		return "", fmt.Errorf("timed out waiting for new instance in %s", asgName)
	}

	// Get private IP.
	ip, err := l.instancePrivateIP(ctx, newInstanceID)
	if err != nil {
		return "", fmt.Errorf("get IP for %s: %w", newInstanceID, err)
	}
	addr := fmt.Sprintf("%s:%d", ip, l.port)

	l.mu.Lock()
	l.instances[addr] = newInstanceID
	l.known[newInstanceID] = true
	l.mu.Unlock()

	log.Printf("asg-launcher: instance %s ready at %s", newInstanceID, addr)
	return addr, nil
}

func (l *ASGLauncher) TerminateInstance(ctx context.Context, addr string) error {
	l.mu.Lock()
	instanceID, ok := l.instances[addr]
	delete(l.instances, addr)
	delete(l.known, instanceID)
	l.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown instance for addr %s", addr)
	}

	shouldDecrement := true
	log.Printf("asg-launcher: terminating instance %s (%s)", instanceID, addr)
	_, err := l.asgClient.TerminateInstanceInAutoScalingGroup(ctx,
		&autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     &instanceID,
			ShouldDecrementDesiredCapacity: &shouldDecrement,
		})
	return err
}

func (l *ASGLauncher) asgInstanceIDs(ctx context.Context, asgName string) ([]string, error) {
	out, err := l.asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return nil, err
	}
	if len(out.AutoScalingGroups) == 0 {
		return nil, fmt.Errorf("ASG %s not found", asgName)
	}

	var ids []string
	for _, inst := range out.AutoScalingGroups[0].Instances {
		if inst.LifecycleState == astypes.LifecycleStateInService && inst.InstanceId != nil {
			ids = append(ids, *inst.InstanceId)
		}
	}
	return ids, nil
}

func (l *ASGLauncher) instancePrivateIP(ctx context.Context, instanceID string) (string, error) {
	out, err := l.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
		Filters: []ec2types.Filter{{
			Name:   strPtr("instance-state-name"),
			Values: []string{"running"},
		}},
	})
	if err != nil {
		return "", err
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if inst.PrivateIpAddress != nil {
				return *inst.PrivateIpAddress, nil
			}
		}
	}
	return "", fmt.Errorf("no private IP for instance %s", instanceID)
}

func (l *ASGLauncher) tierNames() []string {
	var names []string
	for k := range l.tiers {
		names = append(names, k)
	}
	return names
}

func strPtr(s string) *string { return &s }
