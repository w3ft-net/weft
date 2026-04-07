package coordinator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// ASGLauncher implements ShardLauncher by scaling AWS Auto Scaling Groups.
// It maps tier names to ASG names and uses SetDesiredCapacity to provision
// new instances.
type ASGLauncher struct {
	asgClient *autoscaling.Client
	ec2Client *ec2.Client
	tiers     map[string]string // tier → ASG name
	port      int
	mu        sync.Mutex
	instances map[string]string // addr → instance ID (for surgical termination)
	known     map[string]bool   // instance IDs already claimed
}

// NewASGLauncher creates a launcher for the given tier→ASG name mapping.
func NewASGLauncher(region string, tiers map[string]string, port int) (*ASGLauncher, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &ASGLauncher{
		asgClient: autoscaling.NewFromConfig(cfg),
		ec2Client: ec2.NewFromConfig(cfg),
		tiers:     tiers,
		port:      port,
		instances: make(map[string]string),
		known:     make(map[string]bool),
	}, nil
}

func (l *ASGLauncher) Launch(ctx context.Context, tier string, cfg *pb.ConfigureRequest) (string, error) {
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

	// Also check current ASG members.
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
	for i := 0; i < 60; i++ { // up to 5 minutes
		time.Sleep(5 * time.Second)
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

	// Wait for gRPC to be reachable and call Configure.
	for i := 0; i < 24; i++ { // up to 2 more minutes
		time.Sleep(5 * time.Second)
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			continue
		}
		if cfg != nil {
			client := pb.NewShardQueryClient(conn)
			if _, err := client.Configure(ctx, cfg); err != nil {
				conn.Close()
				continue
			}
		}
		conn.Close()

		l.mu.Lock()
		l.instances[addr] = newInstanceID
		l.known[newInstanceID] = true
		l.mu.Unlock()

		log.Printf("asg-launcher: instance %s ready at %s", newInstanceID, addr)
		return addr, nil
	}

	return "", fmt.Errorf("timed out waiting for gRPC on %s (instance %s)", addr, newInstanceID)
}

func (l *ASGLauncher) Terminate(ctx context.Context, addr string) error {
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

// asgInstanceIDs returns InService instance IDs for the named ASG.
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
		if inst.LifecycleState == "InService" && inst.InstanceId != nil {
			ids = append(ids, *inst.InstanceId)
		}
	}
	return ids, nil
}

// instancePrivateIP returns the private IP for an EC2 instance.
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
