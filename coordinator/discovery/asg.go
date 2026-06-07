package discovery

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/graybeardcorp/weft/shard"
)

// ASGDiscovery discovers shard or compute instances from an AWS Auto Scaling Group.
type ASGDiscovery struct {
	asgClient *autoscaling.Client
	ec2Client *ec2.Client
	asgName   string
	port      int
}

// NewASGDiscovery creates a discovery client for the given ASG.
func NewASGDiscovery(region, asgName string, port int) (*ASGDiscovery, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &ASGDiscovery{
		asgClient: autoscaling.NewFromConfig(cfg),
		ec2Client: ec2.NewFromConfig(cfg),
		asgName:   asgName,
		port:      port,
	}, nil
}

// DiscoverAddrs returns the private IP:port addresses of healthy instances in the ASG.
func (d *ASGDiscovery) DiscoverAddrs(ctx context.Context) ([]string, error) {
	// Get instance IDs from ASG.
	asgOut, err := d.asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{d.asgName},
	})
	if err != nil {
		return nil, fmt.Errorf("describe ASG %s: %w", d.asgName, err)
	}
	if len(asgOut.AutoScalingGroups) == 0 {
		return nil, fmt.Errorf("ASG %s not found", d.asgName)
	}

	var instanceIDs []string
	for _, inst := range asgOut.AutoScalingGroups[0].Instances {
		if inst.LifecycleState == "InService" && inst.InstanceId != nil {
			instanceIDs = append(instanceIDs, *inst.InstanceId)
		}
	}

	if len(instanceIDs) == 0 {
		return nil, nil
	}

	// Get private IPs from EC2.
	ec2Out, err := d.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
		Filters: []ec2types.Filter{{
			Name:   strPtr("instance-state-name"),
			Values: []string{"running"},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe instances: %w", err)
	}

	var addrs []string
	for _, res := range ec2Out.Reservations {
		for _, inst := range res.Instances {
			if inst.PrivateIpAddress != nil {
				addrs = append(addrs, fmt.Sprintf("%s:%d", *inst.PrivateIpAddress, d.port))
			}
		}
	}

	return addrs, nil
}

// DiscoverShards returns Shard interfaces connected to each instance in the ASG.
// Each shard gets a wide time window (will be narrowed by the shard's own config).
// shardSecret is the shared bearer attached to every outbound call so the
// shard's SharedTokenServerInterceptor accepts it.
func (d *ASGDiscovery) DiscoverShards(ctx context.Context, shardSecret string) ([]shard.Shard, error) {
	addrs, err := d.DiscoverAddrs(ctx)
	if err != nil {
		return nil, err
	}

	// For now, assign wide time windows. In production the coordinator
	var shards []shard.Shard
	for i, addr := range addrs {
		id := fmt.Sprintf("shard-%d", i)
		gs, err := shard.NewGRPCShard(id, addr, shardSecret)
		if err != nil {
			return nil, fmt.Errorf("connect to shard %s at %s: %w", id, addr, err)
		}
		shards = append(shards, gs)
	}

	return shards, nil
}

func strPtr(s string) *string { return &s }
