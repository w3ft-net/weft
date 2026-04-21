package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/kevlarcon/weft/store"
)

// LifecycleEvent is the subset of the ASG launch lifecycle notification
// that the coordinator cares about. Both the SNS-payload shape and
// the raw hook shape carry these fields; our SQS subscriber unwraps
// SNS before dispatching.
type LifecycleEvent struct {
	ASGName              string    `json:"AutoScalingGroupName"`
	InstanceID           string    `json:"EC2InstanceId"`
	LifecycleHookName    string    `json:"LifecycleHookName"`
	LifecycleActionToken string    `json:"LifecycleActionToken"`
	LifecycleTransition  string    `json:"LifecycleTransition"` // autoscaling:EC2_INSTANCE_LAUNCHING
	Time                 time.Time `json:"Time"`
}

// LifecycleDecision is what HandleLaunchEvent concluded about a
// pending instance. Exposed for tests and for the SQS poller to act on.
type LifecycleDecision string

const (
	DecisionChristen LifecycleDecision = "CHRISTEN" // new volume created + attached
	DecisionRecover  LifecycleDecision = "RECOVER"  // existing volume reattached
	DecisionAbandon  LifecycleDecision = "ABANDON"  // couldn't decide safely; ASG should terminate
)

// EC2InstancesAPI is the subset of the EC2 client needed by the
// lifecycle handler to look up an instance's AZ.
type EC2InstancesAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// ASGLifecycleAPI is the subset of the ASG client the handler uses to
// signal completion of the Pending:Wait phase. We already have a
// broader ASGApi interface but this narrower one fits unit tests
// better.
type ASGLifecycleAPI interface {
	CompleteLifecycleAction(ctx context.Context, in *autoscaling.CompleteLifecycleActionInput, opts ...func(*autoscaling.Options)) (*autoscaling.CompleteLifecycleActionOutput, error)
}

// LifecycleHandler processes ASG launch hook events. On each event it
// decides whether the new instance is a christening (fresh shard) or
// a recovery (replacing a dead instance for an existing shard), then
// creates/attaches the appropriate EBS volume and signals
// CompleteLifecycleAction.
type LifecycleHandler struct {
	volumes      VolumeManager
	ec2Instances EC2InstancesAPI
	asg          ASGLifecycleAPI
	store        HeartbeatStore    // used to decide whether a "prior owner" instance is really dead
	shardOps     ShardOpsStore     // for claiming LAUNCHING rows on CHRISTEN
	registry     *ShardRegistry    // for the same decision — only shards we know about are eligible
	tierByASG    map[string]string // ASG name → tier (small/medium/large)
	sizeByTier   map[string]int32  // tier → data_volume_size_gb
	deviceName   string            // e.g. /dev/sdf
	deadFor      time.Duration     // "no heartbeat in ≥ deadFor" counts as dead
	defaultAZ    string            // safety net: if DescribeInstances fails we still need an AZ
}

// LifecycleHandlerConfig wraps the constructor args so we don't end
// up with a 10-arg function.
type LifecycleHandlerConfig struct {
	Volumes      VolumeManager
	EC2Instances EC2InstancesAPI
	ASG          ASGLifecycleAPI
	Store        HeartbeatStore
	ShardOps     ShardOpsStore
	Registry     *ShardRegistry
	TierByASG    map[string]string
	SizeByTier   map[string]int32
	DeviceName   string
	DeadFor      time.Duration
}

// NewLifecycleHandler constructs a handler. Defaults:
//   - DeviceName  = /dev/sdf
//   - DeadFor     = 2 minutes
func NewLifecycleHandler(cfg LifecycleHandlerConfig) *LifecycleHandler {
	h := &LifecycleHandler{
		volumes:      cfg.Volumes,
		ec2Instances: cfg.EC2Instances,
		asg:          cfg.ASG,
		store:        cfg.Store,
		shardOps:     cfg.ShardOps,
		registry:     cfg.Registry,
		tierByASG:    cfg.TierByASG,
		sizeByTier:   cfg.SizeByTier,
		deviceName:   cfg.DeviceName,
		deadFor:      cfg.DeadFor,
	}
	if h.deviceName == "" {
		h.deviceName = "/dev/sdf"
	}
	if h.deadFor == 0 {
		h.deadFor = 2 * time.Minute
	}
	return h
}

// HandleLaunchEvent processes one lifecycle event end-to-end: looks up
// the instance's AZ, decides christen/recover/abandon, creates or
// attaches the volume, signals the ASG. Returns the decision so the
// SQS poller can log + metric on the outcome.
func (h *LifecycleHandler) HandleLaunchEvent(ctx context.Context, ev LifecycleEvent) (LifecycleDecision, error) {
	if ev.LifecycleTransition != "" && ev.LifecycleTransition != "autoscaling:EC2_INSTANCE_LAUNCHING" {
		// Terminating hooks aren't wired yet; ignore.
		return DecisionAbandon, fmt.Errorf("ignored transition %q", ev.LifecycleTransition)
	}
	tier, ok := h.tierByASG[ev.ASGName]
	if !ok {
		return DecisionAbandon, fmt.Errorf("unknown ASG %q (not a shard tier)", ev.ASGName)
	}

	az, err := h.lookupInstanceAZ(ctx, ev.InstanceID)
	if err != nil {
		return DecisionAbandon, fmt.Errorf("lookup AZ for %s: %w", ev.InstanceID, err)
	}

	decision, volumeID, err := h.decide(ctx, ev.InstanceID, tier, az)
	if err != nil {
		log.Printf("lifecycle: decide %s/%s: %v", tier, ev.InstanceID, err)
		h.signal(ctx, ev, "ABANDON")
		return DecisionAbandon, err
	}

	if err := h.volumes.AttachToInstance(ctx, volumeID, ev.InstanceID, h.deviceName); err != nil {
		log.Printf("lifecycle: attach %s → %s: %v", volumeID, ev.InstanceID, err)
		h.signal(ctx, ev, "ABANDON")
		return DecisionAbandon, err
	}

	log.Printf("lifecycle: %s instance %s with volume %s (tier=%s az=%s)",
		decision, ev.InstanceID, volumeID, tier, az)
	h.signal(ctx, ev, "CONTINUE")
	return decision, nil
}

// decide runs the christen-vs-recover-vs-abandon tree. Returns the
// volume id we committed to (created or found) so the caller can
// attach it.
//
// Priority order, by design:
//  1. Durability: a recoverable orphan volume (exactly one, tagged
//     with a shard_id we recognise as dead) always wins. Its data
//     needs to come back online; any alternative strands it.
//  2. Reconciler intent: with no recoverable volume, claim a
//     pending LAUNCHING row and christen.
//  3. Abandon: ambiguous recovery or no pending row for a
//     christen. ASG terminates the instance.
//
// The subtle case is "pending LAUNCHING row AND orphan volume."
// We choose RECOVER — the reconciler's LAUNCHING row stays
// unclaimed, which is benign (next tick's syncASGDesiredCapacity
// sees the still-pending row and brings up another instance for it).
// Inverting this (preferring the LAUNCHING row) would let the
// orphan volume sit unattached and eventually get reaped or
// deleted, destroying the shard's data. See cleanup.md §7.
func (h *LifecycleHandler) decide(ctx context.Context, instanceID, tier, az string) (LifecycleDecision, string, error) {
	candidates, err := h.volumes.ListAvailableByTier(ctx, tier, az)
	if err != nil {
		return DecisionAbandon, "", err
	}

	// Filter to volumes whose shard id we actually recognize as
	// "live" in the registry/store. Untagged volumes (shard_id
	// absent) are christening leftovers and are not recovery
	// candidates; tagged-but-unknown ones are abandoned cruft
	// belonging to a shard we've already torn down.
	var recoverable []VolumeInfo
	for _, v := range candidates {
		if v.ShardID == "" {
			continue
		}
		if !h.recognisedAsDead(v.ShardID) {
			continue
		}
		recoverable = append(recoverable, v)
	}

	switch len(recoverable) {
	case 1:
		return DecisionRecover, recoverable[0].VolumeID, nil
	case 0:
		size, ok := h.sizeByTier[tier]
		if !ok || size <= 0 {
			return DecisionAbandon, "", fmt.Errorf("no data_volume_size_gb configured for tier %q", tier)
		}
		// CHRISTEN requires a reconciler-minted LAUNCHING row
		// naming this tier. The row becomes the volume's shard_id
		// tag — no placeholder, no re-tag post-boot. An instance
		// reaching this branch without a row is an orphan (ASG
		// scaled out-of-band, or reconciler/ASG state diverged):
		// ABANDON so the ASG terminates it.
		if h.shardOps == nil {
			return DecisionAbandon, "", fmt.Errorf("christen for %s: no ShardOps store — can't claim LAUNCHING row", instanceID)
		}
		claimed, claimOK, err := h.shardOps.ClaimLaunchingForTier(tier, instanceID)
		if err != nil {
			return DecisionAbandon, "", fmt.Errorf("claim LAUNCHING row for %s/%s: %w", tier, instanceID, err)
		}
		if !claimOK {
			return DecisionAbandon, "", fmt.Errorf("no LAUNCHING row for tier %s; instance %s is an orphan", tier, instanceID)
		}
		log.Printf("lifecycle: claimed LAUNCHING row %s for instance %s (tier=%s)",
			claimed.ID, instanceID, tier)
		id, err := h.volumes.CreateForShard(ctx, CreateVolumeOpts{
			ShardID: claimed.ID,
			Tier:    tier,
			AZ:      az,
			SizeGB:  size,
		})
		if err != nil {
			return DecisionAbandon, "", fmt.Errorf("create volume for new shard: %w", err)
		}
		return DecisionChristen, id, nil
	default:
		ids := make([]string, 0, len(recoverable))
		for _, v := range recoverable {
			ids = append(ids, v.VolumeID+"/"+v.ShardID)
		}
		return DecisionAbandon, "", fmt.Errorf("ambiguous recovery: %d candidate volumes in %s for %s (%v)",
			len(recoverable), az, tier, ids)
	}
}

// recognisedAsDead returns true when the given shard id names a shard
// the coordinator still considers hot (in the registry) AND whose
// last instance's heartbeat is stale enough that we're comfortable
// yanking the volume out from under it.
//
// For MVP: if the shard is in the registry and has no recent
// heartbeat, we consider it dead. A more sophisticated check (cross-
// referencing the instance id, confirming the instance is actually
// terminated) is possible but harder to unit-test.
func (h *LifecycleHandler) recognisedAsDead(shardID string) bool {
	if h.registry == nil {
		return false
	}
	entry := h.registry.Get(shardID)
	if entry == nil {
		return false
	}
	if entry.State == StateArchived || entry.State == StateExpiring {
		return false
	}
	// If the registry thinks the shard is still live at an addr, and
	// we have a recent heartbeat, the prior instance isn't dead yet —
	// leave it alone.
	if h.store == nil {
		return true
	}
	heartbeats, err := h.store.LiveShardHeartbeats(fmt.Sprintf("%d seconds", int(h.deadFor.Seconds())))
	if err != nil {
		return true // we'd rather recover than leave a shard stranded on a store hiccup
	}
	for _, hb := range heartbeats {
		if hb.ShardID == shardID {
			return false
		}
	}
	return true
}

// signal calls CompleteLifecycleAction. Best-effort — if the ASG call
// fails the instance stays in Pending:Wait until the hook timeout,
// which is still better than crashing the handler.
func (h *LifecycleHandler) signal(ctx context.Context, ev LifecycleEvent, result string) {
	if h.asg == nil {
		return
	}
	input := &autoscaling.CompleteLifecycleActionInput{
		AutoScalingGroupName:  aws.String(ev.ASGName),
		LifecycleHookName:     aws.String(ev.LifecycleHookName),
		LifecycleActionResult: aws.String(result),
	}
	if ev.LifecycleActionToken != "" {
		input.LifecycleActionToken = aws.String(ev.LifecycleActionToken)
	} else {
		// Token path is preferred when available, but we can fall
		// back to InstanceId for cases where the hook was configured
		// without a token.
		input.InstanceId = aws.String(ev.InstanceID)
	}
	if _, err := h.asg.CompleteLifecycleAction(ctx, input); err != nil {
		log.Printf("lifecycle: CompleteLifecycleAction(%s, %s): %v", ev.InstanceID, result, err)
	}
}

func (h *LifecycleHandler) lookupInstanceAZ(ctx context.Context, instanceID string) (string, error) {
	out, err := h.ec2Instances.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", err
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.Placement != nil && inst.Placement.AvailabilityZone != nil {
				return *inst.Placement.AvailabilityZone, nil
			}
		}
	}
	return "", fmt.Errorf("no AZ found for instance %s", instanceID)
}

// ParseLifecycleEvent unwraps an SNS-wrapped ASG lifecycle payload.
// SNS wraps the actual hook JSON in an outer envelope with the hook
// payload stringified under the `Message` key. If the input isn't
// SNS-wrapped (raw hook delivery), we fall back to parsing it
// directly.
func ParseLifecycleEvent(body []byte) (LifecycleEvent, error) {
	var envelope struct {
		Type    string `json:"Type"`
		Message string `json:"Message"`
	}
	var ev LifecycleEvent
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Type == "Notification" && envelope.Message != "" {
		if err := json.Unmarshal([]byte(envelope.Message), &ev); err != nil {
			return ev, fmt.Errorf("parse SNS inner: %w", err)
		}
		return ev, nil
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return ev, fmt.Errorf("parse lifecycle payload: %w", err)
	}
	return ev, nil
}

// assert that HeartbeatStore still supports the interval shape we pass.
var _ = store.ShardHeartbeat{}
