package logdb

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// ── Mock ASG client ──────────────────────────────────────────

type mockASG struct {
	lastSetDesired *int32
	lastSetASGName string
	setCapErr      error
	maxSize        int32
	describeErr    error
	terminateErr   error
	terminated     []string // instance IDs passed to TerminateInstanceInAutoScalingGroup
	// Last ShouldDecrementDesiredCapacity flag seen. Lets tests
	// distinguish the §4 "decrement" call from the recovery-replace
	// call with one scalar field.
	lastTerminateDecrement bool
}

func (m *mockASG) SetDesiredCapacity(_ context.Context, in *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	if m.setCapErr != nil {
		return nil, m.setCapErr
	}
	if in != nil {
		if in.AutoScalingGroupName != nil {
			m.lastSetASGName = *in.AutoScalingGroupName
		}
		if in.DesiredCapacity != nil {
			v := *in.DesiredCapacity
			m.lastSetDesired = &v
		}
	}
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (m *mockASG) DescribeAutoScalingGroups(_ context.Context, input *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	if len(input.AutoScalingGroupNames) == 0 {
		return nil, fmt.Errorf("no ASG name")
	}
	name := input.AutoScalingGroupNames[0]
	max := m.maxSize
	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []astypes.AutoScalingGroup{{
			AutoScalingGroupName: &name,
			MaxSize:              &max,
		}},
	}, nil
}

func (m *mockASG) TerminateInstanceInAutoScalingGroup(_ context.Context, input *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	if input != nil {
		if input.InstanceId != nil {
			m.terminated = append(m.terminated, *input.InstanceId)
		}
		if input.ShouldDecrementDesiredCapacity != nil {
			m.lastTerminateDecrement = *input.ShouldDecrementDesiredCapacity
		}
	}
	return nil, m.terminateErr
}

// mockEC2 is retained for the Terminate tests that don't need instance
// lookups.
type mockEC2 struct{}

func (m *mockEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

// ── Tests ────────────────────────────────────────────────────

// LaunchInstance on ASG is a stale path under §3 — the reconciler
// mints LAUNCHING rows and syncASGDesiredCapacity drives ASG scaling
// absolutely. Any caller still invoking LaunchInstance on ASG is
// a bug; the method returns an error to surface that.
func TestASGLauncher_LaunchInstance_IsStaleAndErrors(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)
	_, err := launcher.LaunchInstance(context.Background(), "small")
	if err == nil {
		t.Fatal("expected LaunchInstance to error (§3 routes through SetDesiredCapacity)")
	}
}

// SetDesiredCapacity passes through to the ASG client with the
// correct ASG name (looked up by tier) and desired value.
func TestASGLauncher_SetDesiredCapacity(t *testing.T) {
	asg := &mockASG{}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "weft-shard-small"}, 9000)

	if err := launcher.SetDesiredCapacity(context.Background(), "small", 5); err != nil {
		t.Fatalf("SetDesiredCapacity: %v", err)
	}
	if asg.lastSetASGName != "weft-shard-small" {
		t.Errorf("ASG name: got %q want weft-shard-small", asg.lastSetASGName)
	}
	if asg.lastSetDesired == nil || *asg.lastSetDesired != 5 {
		t.Errorf("desired: got %v want 5", asg.lastSetDesired)
	}
}

func TestASGLauncher_SetDesiredCapacity_UnknownTier(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "weft-shard-small"}, 9000)
	err := launcher.SetDesiredCapacity(context.Background(), "xlarge", 1)
	if err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestASGLauncher_SetDesiredCapacity_AWSError(t *testing.T) {
	asg := &mockASG{setCapErr: fmt.Errorf("throttled")}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "weft-shard-small"}, 9000)
	if err := launcher.SetDesiredCapacity(context.Background(), "small", 3); err == nil {
		t.Fatal("expected AWS error to surface")
	}
}

// TierMaxSize reads MaxSize from DescribeAutoScalingGroups — the
// clamp in syncASGDesiredCapacity uses it.
func TestASGLauncher_TierMaxSize(t *testing.T) {
	asg := &mockASG{maxSize: 8}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "weft-shard-small"}, 9000)

	got, err := launcher.TierMaxSize(context.Background(), "small")
	if err != nil {
		t.Fatalf("TierMaxSize: %v", err)
	}
	if got != 8 {
		t.Errorf("TierMaxSize: got %d want 8", got)
	}
}

func TestASGLauncher_TierMaxSize_UnknownTier(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "weft-shard-small"}, 9000)
	if _, err := launcher.TierMaxSize(context.Background(), "xlarge"); err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

// TerminateInstance with an explicit id (the common case post-§3
// where shard_records.InstanceID is the source of truth) must
// succeed and forward the id to the ASG API.
func TestASGLauncher_Terminate_AdoptedByID(t *testing.T) {
	asg := &mockASG{}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)

	err := launcher.TerminateInstance(context.Background(), "i-adopted-01", "172.31.128.135:9000")
	if err != nil {
		t.Fatalf("expected successful terminate when id is provided, got %v", err)
	}
	if len(asg.terminated) != 1 {
		t.Fatalf("expected 1 TerminateInstanceInAutoScalingGroup call, got %d: %v",
			len(asg.terminated), asg.terminated)
	}
	if asg.terminated[0] != "i-adopted-01" {
		t.Errorf("expected TerminateInstanceInAutoScalingGroup(i-adopted-01), got %s", asg.terminated[0])
	}
}

// TerminateForReplacement must pass ShouldDecrementDesiredCapacity=false
// so the ASG launches a replacement rather than shrinking. Opposite
// of TerminateInstance's semantics.
func TestASGLauncher_TerminateForReplacement_KeepsDesiredCapacity(t *testing.T) {
	asg := &mockASG{}
	launcher := NewASGLauncherWithClients(asg, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)

	if err := launcher.TerminateForReplacement(context.Background(), "i-dead"); err != nil {
		t.Fatalf("TerminateForReplacement: %v", err)
	}
	if len(asg.terminated) != 1 || asg.terminated[0] != "i-dead" {
		t.Errorf("terminated: got %v, want [i-dead]", asg.terminated)
	}
	if asg.lastTerminateDecrement {
		t.Error("ShouldDecrementDesiredCapacity must be false for replacement-semantics terminate")
	}
}

func TestASGLauncher_TerminateForReplacement_RequiresInstanceID(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)
	if err := launcher.TerminateForReplacement(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty instance id")
	}
}

// TerminateInstance with no id and no known addr→id mapping must
// error rather than silently no-op.
func TestASGLauncher_Terminate_Unknown(t *testing.T) {
	launcher := NewASGLauncherWithClients(&mockASG{}, &mockEC2{}, map[string]string{"small": "my-asg"}, 9000)

	err := launcher.TerminateInstance(context.Background(), "", "10.0.0.1:9000")
	if err == nil {
		t.Fatal("expected error for unknown instance")
	}
}
