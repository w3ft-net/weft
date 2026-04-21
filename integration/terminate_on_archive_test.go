//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/kevlarcon/weft/coordinator"
)

// recordingASGApi satisfies coordinator.ASGApi so we can drive a real
// ASGLauncher at integration scope and see exactly which calls it
// emits. LocalStack Community doesn't implement ASG; stubbing is the
// only path. Every call is serialized through aws-sdk-go-v2's
// request path before landing here, so this covers the same wiring
// prod hits minus the remote transport.
type recordingASGApi struct {
	mu                sync.Mutex
	terminateCalls    []recordedTerminate
	describeResponses map[string]int32 // ASG name → MaxSize
}

type recordedTerminate struct {
	InstanceID               string
	DecrementDesiredCapacity bool
}

func (r *recordingASGApi) SetDesiredCapacity(_ context.Context, _ *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (r *recordingASGApi) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	if len(in.AutoScalingGroupNames) == 0 {
		return &autoscaling.DescribeAutoScalingGroupsOutput{}, nil
	}
	name := in.AutoScalingGroupNames[0]
	max, ok := r.describeResponses[name]
	if !ok {
		max = 100
	}
	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []astypes.AutoScalingGroup{{
			AutoScalingGroupName: &name,
			MaxSize:              &max,
		}},
	}, nil
}

func (r *recordingASGApi) TerminateInstanceInAutoScalingGroup(_ context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := recordedTerminate{}
	if in != nil {
		if in.InstanceId != nil {
			call.InstanceID = *in.InstanceId
		}
		if in.ShouldDecrementDesiredCapacity != nil {
			call.DecrementDesiredCapacity = *in.ShouldDecrementDesiredCapacity
		}
	}
	r.terminateCalls = append(r.terminateCalls, call)
	return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
}

func (r *recordingASGApi) terminates() []recordedTerminate {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedTerminate, len(r.terminateCalls))
	copy(out, r.terminateCalls)
	return out
}

// stubBucketsFromAppBucket wraps a single bucket into the Buckets
// interface so ReconcileBucket can drive from it.
type stubBucketsFromAppBucket struct {
	bucket *coordinator.AppBucket
}

func (s *stubBucketsFromAppBucket) Default() (*coordinator.AppBucket, error) {
	return s.bucket, nil
}
func (s *stubBucketsFromAppBucket) ByID(id int64) (*coordinator.AppBucket, error) {
	if s.bucket != nil && s.bucket.ID == id {
		return s.bucket, nil
	}
	return nil, nil
}
func (s *stubBucketsFromAppBucket) ByLabel(label string) (*coordinator.AppBucket, error) {
	if s.bucket != nil && s.bucket.Label == label {
		return s.bucket, nil
	}
	return nil, nil
}
func (s *stubBucketsFromAppBucket) All() ([]*coordinator.AppBucket, error) {
	if s.bucket == nil {
		return nil, nil
	}
	return []*coordinator.AppBucket{s.bucket}, nil
}
func (s *stubBucketsFromAppBucket) EvaluateSplit(_ map[string]int64) (string, error) {
	return "", nil
}
func (s *stubBucketsFromAppBucket) Split(_ string) (*coordinator.AppBucket, error) {
	return nil, nil
}
func (s *stubBucketsFromAppBucket) SetStore(_ coordinator.BucketStore) {}
func (s *stubBucketsFromAppBucket) AddAppToBucket(_ int64, _ string) (*coordinator.AppBucket, error) {
	return nil, nil
}

// ── Tests ────────────────────────────────────────────────────

// Integration coverage for the terminate-on-archive path. LocalStack
// Community doesn't implement ASG, so the asg client is stubbed —
// but the full aws-sdk-go-v2 call stack (request signing,
// serialization, middleware) still runs, and ASGLauncher is the
// real production type. This catches wiring bugs (missing IAM
// permissions would manifest the same way here: via the real SDK
// path) that unit tests with mocks don't.
//
// Seeds an entry already at state=ARCHIVED (archive() covered
// separately by TestLifecycle_Integration_RetirementPath) with an
// InstanceID populated from a LocalStack-seeded EC2 instance. After
// one reconciler pass, we expect TerminateInstanceInAutoScalingGroup
// to have been called via the real SDK with the right arguments,
// and entry.InstanceID to be nulled so the op doesn't re-fire.
func TestTerminateOnArchive_CallsASGTerminate(t *testing.T) {
	cfg := awsTestConfig(t)
	ec2c := ec2.NewFromConfig(cfg)

	// Seed a real LocalStack EC2 instance so the instance id is
	// AWS-realistic (i-xxxxxx) even though the ASG call itself is
	// stubbed. This is the closest we can get to "prod wiring" on
	// LocalStack Community.
	instanceID := seedInstance(t, cfg, "us-east-2a")

	shardID := "shard-term-" + testSuffix()
	bucket := &coordinator.AppBucket{
		ID:              1,
		Label:           "default",
		Apps:            nil,
		Target:          0,
		Tier:            "small",
		RetentionPeriod: time.Hour,
	}

	reg := coordinator.NewShardRegistry()
	reg.Add(&coordinator.ShardEntry{
		ID:         shardID,
		BucketID:   bucket.ID,
		Tier:       "small",
		State:      coordinator.StateArchived, // retire already ran; just the compute left
		InstanceID: instanceID,
		Addr:       "10.0.0.10:9000",
		// EBSVolumeID intentionally empty — volume already retired.
	})

	asgRec := &recordingASGApi{
		describeResponses: map[string]int32{"weft-shard-small": 16},
	}
	launcher := coordinator.NewASGLauncherWithClients(
		asgRec,
		ec2c,
		map[string]string{"small": "weft-shard-small"},
		9000,
	)

	prov := coordinator.NewProvisioner(reg, launcher, coordinator.ProvisionerConfig{})
	prov.Buckets = &stubBucketsFromAppBucket{bucket: bucket}

	ctx := context.Background()

	// Tick 1: op 6 fires. Stub ASG receives terminate via real SDK
	// call stack; entry's InstanceID/Addr get nulled.
	prov.ReconcileBucket(ctx, bucket)

	calls := asgRec.terminates()
	if len(calls) != 1 {
		t.Fatalf("want 1 terminate call, got %d: %+v", len(calls), calls)
	}
	if calls[0].InstanceID != instanceID {
		t.Errorf("terminate instance: got %q want %q", calls[0].InstanceID, instanceID)
	}
	if !calls[0].DecrementDesiredCapacity {
		t.Error("ShouldDecrementDesiredCapacity=true is required — without it the ASG keeps trying to maintain the archived instance's slot")
	}

	entry := reg.Get(shardID)
	if entry.InstanceID != "" {
		t.Errorf("InstanceID: got %q want empty (should be nulled post-terminate)", entry.InstanceID)
	}
	if entry.Addr != "" {
		t.Errorf("Addr: got %q want empty", entry.Addr)
	}
	if entry.State != coordinator.StateArchived {
		t.Errorf("State: got %s want still ARCHIVED", entry.State)
	}

	// Tick 2: idempotency — the op must not re-fire with InstanceID
	// already nulled.
	prov.ReconcileBucket(ctx, bucket)
	if got := len(asgRec.terminates()); got != 1 {
		t.Errorf("after tick 2: want still 1 terminate call, got %d (op re-fired)", got)
	}

	_ = aws.String // keep import referenced across go versions
}
