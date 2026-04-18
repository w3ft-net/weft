//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/kevlarcon/weft/coordinator"
)

// LocalStack endpoint from docker-compose. Set LOCALSTACK_ENDPOINT to
// override; otherwise defaults to the docker-compose mapping.
const localStackEndpoint = "http://localhost:4566"

// awsTestConfig returns an aws.Config wired to LocalStack with throwaway
// credentials.
func awsTestConfig(t *testing.T) aws.Config {
	t.Helper()
	endpoint := os.Getenv("LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		endpoint = localStackEndpoint
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-2"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		awsconfig.WithBaseEndpoint(endpoint),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return cfg
}

// recordingASG satisfies ASGLifecycleAPI while recording CompleteLifecycleAction
// calls. LocalStack Community doesn't implement ASG, so we stub just
// this one call.
type recordingASG struct {
	mu    sync.Mutex
	calls []string // "CONTINUE:i-xxx" / "ABANDON:i-xxx"
}

func (r *recordingASG) CompleteLifecycleAction(_ context.Context, in *autoscaling.CompleteLifecycleActionInput, _ ...func(*autoscaling.Options)) (*autoscaling.CompleteLifecycleActionOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := ""
	if in.LifecycleActionResult != nil {
		result = *in.LifecycleActionResult
	}
	instance := ""
	if in.InstanceId != nil {
		instance = *in.InstanceId
	}
	r.calls = append(r.calls, result+":"+instance)
	return &autoscaling.CompleteLifecycleActionOutput{}, nil
}

func (r *recordingASG) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// setupQueue creates an SNS topic, an SQS queue, subscribes the queue
// to the topic, and returns (topicArn, queueURL).
func setupQueue(t *testing.T, cfg aws.Config, name string) (string, string) {
	t.Helper()
	snsClient := sns.NewFromConfig(cfg)
	sqsClient := sqs.NewFromConfig(cfg)

	topicOut, err := snsClient.CreateTopic(context.Background(), &sns.CreateTopicInput{
		Name: aws.String(name),
	})
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	queueOut, err := sqsClient.CreateQueue(context.Background(), &sqs.CreateQueueInput{
		QueueName: aws.String(name),
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	// Subscribe.
	_, err = snsClient.Subscribe(context.Background(), &sns.SubscribeInput{
		TopicArn: topicOut.TopicArn,
		Protocol: aws.String("sqs"),
		Endpoint: aws.String("arn:aws:sqs:us-east-2:000000000000:" + name),
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return *topicOut.TopicArn, *queueOut.QueueUrl
}

// seedInstance registers a stub instance with LocalStack's EC2 so
// DescribeInstances returns its AZ. LocalStack's RunInstances accepts
// any instance type; the instance isn't actually booted.
func seedInstance(t *testing.T, cfg aws.Config, az string) string {
	t.Helper()
	ec2c := ec2.NewFromConfig(cfg)
	out, err := ec2c.RunInstances(context.Background(), &ec2.RunInstancesInput{
		MaxCount:     aws.Int32(1),
		MinCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      aws.String("ami-test"),
		Placement:    &ec2types.Placement{AvailabilityZone: aws.String(az)},
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	return *out.Instances[0].InstanceId
}

// publishLaunchEvent sends a realistic ASG lifecycle notification to
// the SNS topic. Matches the payload shape AWS emits.
func publishLaunchEvent(t *testing.T, cfg aws.Config, topicArn, asgName, instanceID string) {
	t.Helper()
	event := coordinator.LifecycleEvent{
		ASGName:             asgName,
		InstanceID:          instanceID,
		LifecycleHookName:   asgName + "-launch",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
		Time:                time.Now().UTC(),
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	snsClient := sns.NewFromConfig(cfg)
	_, err = snsClient.Publish(context.Background(), &sns.PublishInput{
		TopicArn: aws.String(topicArn),
		Message:  aws.String(string(body)),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// waitForAttached polls EC2 until the given instance has a non-root
// volume attached, or the deadline expires.
func waitForAttached(t *testing.T, cfg aws.Config, instanceID string, timeout time.Duration) ec2types.InstanceBlockDeviceMapping {
	t.Helper()
	ec2c := ec2.NewFromConfig(cfg)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := ec2c.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err == nil {
			for _, r := range out.Reservations {
				for _, inst := range r.Instances {
					for _, m := range inst.BlockDeviceMappings {
						if m.DeviceName != nil && strings.HasSuffix(*m.DeviceName, "sdf") {
							return m
						}
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("instance %s did not get /dev/sdf attached within %s", instanceID, timeout)
	return ec2types.InstanceBlockDeviceMapping{}
}

// ── Tests ────────────────────────────────────────────────────

// End-to-end christen: publish a launch event, the poller consumes
// it, the handler creates a volume and attaches it, the ASG stub gets
// CONTINUE.
func TestLifecycle_Integration_ChristenPath(t *testing.T) {
	cfg := awsTestConfig(t)
	topic, queue := setupQueue(t, cfg, "lifecycle-christen-"+testSuffix())

	instanceID := seedInstance(t, cfg, "us-east-2a")
	asgName := "weft-shard-small"

	asgRecorder := &recordingASG{}
	reg := coordinator.NewShardRegistry()
	vm := coordinator.NewEC2VolumeManagerWithClient(ec2.NewFromConfig(cfg))

	handler := coordinator.NewLifecycleHandler(coordinator.LifecycleHandlerConfig{
		Volumes:      vm,
		EC2Instances: ec2.NewFromConfig(cfg),
		ASG:          asgRecorder,
		Registry:     reg,
		TierByASG:    map[string]string{asgName: "small"},
		SizeByTier:   map[string]int32{"small": 10},
		DeviceName:   "/dev/sdf",
	})
	poller := coordinator.NewLifecyclePoller(handler, sqs.NewFromConfig(cfg), queue)
	poller.WaitTimeSeconds = 2
	poller.VisibilityTimeout = 30

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poller.Start(ctx)

	publishLaunchEvent(t, cfg, topic, asgName, instanceID)

	// The handler should create a new volume, attach it, and signal
	// CompleteLifecycleAction with CONTINUE.
	mapping := waitForAttached(t, cfg, instanceID, 30*time.Second)
	if mapping.Ebs == nil || mapping.Ebs.VolumeId == nil {
		t.Fatal("no volume attached")
	}

	// The volume should have a shard_tier tag and a placeholder shard_id.
	tags := describeVolumeTags(t, cfg, *mapping.Ebs.VolumeId)
	if tags["shard_tier"] != "small" {
		t.Errorf("shard_tier tag: got %q want small", tags["shard_tier"])
	}
	if !strings.HasPrefix(tags["shard_id"], "pending-") {
		t.Errorf("shard_id tag: got %q want pending-...", tags["shard_id"])
	}

	// Wait for the CONTINUE call.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		calls := asgRecorder.snapshot()
		if len(calls) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	calls := asgRecorder.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CompleteLifecycleAction call, got %d: %v", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "CONTINUE:") {
		t.Errorf("expected CONTINUE, got %q", calls[0])
	}
}

// End-to-end recover: pre-seed an orphan volume tagged with a shard_id
// known to the registry (but not heartbeating), publish a launch
// event, verify the existing volume gets attached rather than a new
// one created.
func TestLifecycle_Integration_RecoverPath(t *testing.T) {
	cfg := awsTestConfig(t)
	topic, queue := setupQueue(t, cfg, "lifecycle-recover-"+testSuffix())

	asgName := "weft-shard-small"
	shardID := "shard-ebs-recover-" + testSuffix()
	az := "us-east-2a"

	// Pre-seed an orphan volume tagged as a dead shard.
	ec2c := ec2.NewFromConfig(cfg)
	volOut, err := ec2c.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int32(10),
		VolumeType:       ec2types.VolumeTypeGp3,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVolume,
			Tags: []ec2types.Tag{
				{Key: aws.String("shard_tier"), Value: aws.String("small")},
				{Key: aws.String("shard_id"), Value: aws.String(shardID)},
			},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	orphanID := *volOut.VolumeId

	instanceID := seedInstance(t, cfg, az)

	reg := coordinator.NewShardRegistry()
	reg.Add(&coordinator.ShardEntry{
		ID:    shardID,
		State: coordinator.StateIngesting,
		Addr:  "10.0.0.1:9000",
	})

	asgRecorder := &recordingASG{}
	vm := coordinator.NewEC2VolumeManagerWithClient(ec2c)
	handler := coordinator.NewLifecycleHandler(coordinator.LifecycleHandlerConfig{
		Volumes:      vm,
		EC2Instances: ec2c,
		ASG:          asgRecorder,
		Registry:     reg,
		// No Store → recognisedAsDead returns true (we'd rather recover than strand).
		TierByASG:  map[string]string{asgName: "small"},
		SizeByTier: map[string]int32{"small": 10},
		DeviceName: "/dev/sdf",
	})
	poller := coordinator.NewLifecyclePoller(handler, sqs.NewFromConfig(cfg), queue)
	poller.WaitTimeSeconds = 2
	poller.VisibilityTimeout = 30

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poller.Start(ctx)

	publishLaunchEvent(t, cfg, topic, asgName, instanceID)

	mapping := waitForAttached(t, cfg, instanceID, 30*time.Second)
	if mapping.Ebs == nil || mapping.Ebs.VolumeId == nil {
		t.Fatal("no volume attached")
	}
	if *mapping.Ebs.VolumeId != orphanID {
		t.Errorf("attached wrong volume: got %s want %s (the orphan)", *mapping.Ebs.VolumeId, orphanID)
	}

	// Confirm no *new* volume was created for shardID — there should
	// be exactly one volume tagged shard_id=<shardID> (the orphan we
	// pre-seeded). LocalStack state persists across tests, so a
	// broader "how many small-tier volumes exist" check catches
	// leftovers from other tests rather than a bug here.
	volumes, err := ec2c.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:shard_id"), Values: []string{shardID}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(volumes.Volumes); n != 1 {
		t.Errorf("expected exactly 1 volume tagged shard_id=%s, got %d", shardID, n)
	}
}

func describeVolumeTags(t *testing.T, cfg aws.Config, volumeID string) map[string]string {
	t.Helper()
	out, err := ec2.NewFromConfig(cfg).DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Volumes) == 0 {
		t.Fatalf("volume %s not found", volumeID)
	}
	tags := map[string]string{}
	for _, tag := range out.Volumes[0].Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}
	return tags
}

// testSuffix returns a short unique string so concurrently-running
// tests (or re-runs against the same LocalStack instance) don't
// collide on SNS/SQS names.
func testSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
}
