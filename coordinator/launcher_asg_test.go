package coordinator

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ── Mock ASG client ──────────────────────────────────────────

type mockASG struct {
	instances []string // current InService instance IDs
	// After SetDesiredCapacity is called, the next DescribeAutoScalingGroups
	// will include newInstanceID (simulating instance launch).
	newInstanceID string
	scaled        atomic.Bool
	setCapErr     error
	describeErr   error
	terminateErr  error
}

func (m *mockASG) SetDesiredCapacity(_ context.Context, _ *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	if m.setCapErr != nil {
		return nil, m.setCapErr
	}
	m.scaled.Store(true)
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (m *mockASG) DescribeAutoScalingGroups(_ context.Context, input *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	if len(input.AutoScalingGroupNames) == 0 {
		return nil, fmt.Errorf("no ASG name")
	}

	var instances []astypes.Instance
	for _, id := range m.instances {
		id := id
		instances = append(instances, astypes.Instance{
			InstanceId:     &id,
			LifecycleState: astypes.LifecycleStateInService,
		})
	}
	// Simulate new instance appearing after scale-up.
	if m.scaled.Load() && m.newInstanceID != "" {
		id := m.newInstanceID
		instances = append(instances, astypes.Instance{
			InstanceId:     &id,
			LifecycleState: astypes.LifecycleStateInService,
		})
	}

	name := input.AutoScalingGroupNames[0]
	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []astypes.AutoScalingGroup{{
			AutoScalingGroupName: &name,
			Instances:            instances,
		}},
	}, nil
}

func (m *mockASG) TerminateInstanceInAutoScalingGroup(_ context.Context, _ *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	return nil, m.terminateErr
}

// ── Mock EC2 client ──────────────────────────────────────────

type mockEC2 struct {
	ips map[string]string // instance ID → private IP
}

func (m *mockEC2) DescribeInstances(_ context.Context, input *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(input.InstanceIds) == 0 {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	id := input.InstanceIds[0]
	ip, ok := m.ips[id]
	if !ok {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{
				InstanceId:       &id,
				PrivateIpAddress: &ip,
			}},
		}},
	}, nil
}

// ── Tests ────────────────────────────────────────────────────

func TestASGLauncher_LaunchInstance(t *testing.T) {
	asg := &mockASG{
		instances:     []string{"i-existing"},
		newInstanceID: "i-new-123",
	}
	ec2c := &mockEC2{
		ips: map[string]string{"i-new-123": "10.0.1.5"},
	}
	launcher := NewASGLauncherWithClients(asg, ec2c, map[string]string{"small": "my-asg"}, 9000)

	addr, err := launcher.LaunchInstance(context.Background(), "small")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.1.5:9000" {
		t.Errorf("expected 10.0.1.5:9000, got %s", addr)
	}
	if !asg.scaled.Load() {
		t.Error("SetDesiredCapacity was not called")
	}
}

func TestASGLauncher_LaunchInstance_UnknownTier(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)
	_, err := launcher.LaunchInstance(context.Background(), "xlarge")
	if err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestASGLauncher_LaunchInstance_SetCapacityError(t *testing.T) {
	asg := &mockASG{setCapErr: fmt.Errorf("access denied")}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)

	_, err := launcher.LaunchInstance(context.Background(), "small")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestASGLauncher_LaunchInstance_Timeout(t *testing.T) {
	// No newInstanceID → will never find a new instance.
	asg := &mockASG{instances: []string{"i-existing"}}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)
	launcher.PollAttempts = 2 // fail fast

	_, err := launcher.LaunchInstance(context.Background(), "small")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestASGLauncher_LaunchInstance_NoIPError(t *testing.T) {
	asg := &mockASG{newInstanceID: "i-new-456"}
	ec2c := &mockEC2{ips: map[string]string{}} // no IP for this instance
	launcher := NewASGLauncherWithClients(asg, ec2c, map[string]string{"small": "my-asg"}, 9000)

	_, err := launcher.LaunchInstance(context.Background(), "small")
	if err == nil {
		t.Fatal("expected error when no IP available")
	}
}

func TestASGLauncher_LaunchInstance_ExistingInstancesFiltered(t *testing.T) {
	// Pre-existing instance should not be returned as the new one.
	asg := &mockASG{
		instances:     []string{"i-old-1", "i-old-2"},
		newInstanceID: "i-brand-new",
	}
	ec2c := &mockEC2{
		ips: map[string]string{"i-brand-new": "10.0.2.1"},
	}
	launcher := NewASGLauncherWithClients(asg, ec2c, map[string]string{"small": "my-asg"}, 9000)

	addr, err := launcher.LaunchInstance(context.Background(), "small")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.2.1:9000" {
		t.Errorf("expected new instance IP, got %s", addr)
	}
}

func TestASGLauncher_Terminate(t *testing.T) {
	asg := &mockASG{newInstanceID: "i-123"}
	ec2c := &mockEC2{ips: map[string]string{"i-123": "10.0.1.1"}}
	launcher := NewASGLauncherWithClients(asg, ec2c, map[string]string{"small": "my-asg"}, 9000)

	// Launch first so we have something to terminate.
	addr, err := launcher.LaunchInstance(context.Background(), "small")
	if err != nil {
		t.Fatal(err)
	}

	err = launcher.TerminateInstance(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
}

func TestASGLauncher_Terminate_Unknown(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)

	err := launcher.TerminateInstance(context.Background(), "10.0.0.1:9000")
	if err == nil {
		t.Fatal("expected error for unknown instance")
	}
}

func TestASGLauncher_Terminate_AWSError(t *testing.T) {
	asg := &mockASG{
		newInstanceID: "i-fail",
		terminateErr:  fmt.Errorf("throttled"),
	}
	ec2c := &mockEC2{ips: map[string]string{"i-fail": "10.0.1.1"}}
	launcher := NewASGLauncherWithClients(asg, ec2c, map[string]string{"small": "my-asg"}, 9000)

	addr, _ := launcher.LaunchInstance(context.Background(), "small")
	err := launcher.TerminateInstance(context.Background(), addr)
	if err == nil {
		t.Fatal("expected error from AWS")
	}
}

func TestASGLauncher_KnownInstancesTracked(t *testing.T) {
	asg := &mockASG{newInstanceID: "i-tracked"}
	ec2c := &mockEC2{ips: map[string]string{"i-tracked": "10.0.1.1"}}
	launcher := NewASGLauncherWithClients(asg, ec2c, map[string]string{"small": "my-asg"}, 9000)

	_, err := launcher.LaunchInstance(context.Background(), "small")
	if err != nil {
		t.Fatal(err)
	}

	launcher.mu.Lock()
	_, tracked := launcher.known["i-tracked"]
	launcher.mu.Unlock()

	if !tracked {
		t.Error("launched instance not tracked in known set")
	}
}
