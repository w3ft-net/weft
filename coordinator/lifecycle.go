package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
//
// Invariant: when Registry is set, Store must also be set. The
// recovery scan checks live heartbeats via Store before yanking a
// volume; without Store, recognisedAsDead would have to guess, and a
// guess of "dead" recovers volumes out from under live shards (the
// 2026-05-03 prod outage). Tests that don't exercise the recovery
// path can pass a nil Registry and nil Store together; production
// callers always supply both.
func NewLifecycleHandler(cfg LifecycleHandlerConfig) *LifecycleHandler {
	if cfg.Registry != nil && cfg.Store == nil {
		panic("coordinator.NewLifecycleHandler: Registry set without Store — recognisedAsDead needs heartbeats to make the recover-vs-leave decision")
	}
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
//
// Logging contract: every event produces (1) an `event` line at entry
// with asg/instance/transition; (2) a `scan` line summarising the
// recovery candidate evaluation (or `decision … reason=…` if we
// short-circuit before scanning); (3) per-candidate `candidate` lines
// when there are any; (4) a final `decision` line with the outcome,
// volume, shard, and reason in key=value form. The orphan-christen
// scenario is grep'able via `decision=ABANDON reason=no-launching-row`.
func (h *LifecycleHandler) HandleLaunchEvent(ctx context.Context, ev LifecycleEvent) (LifecycleDecision, error) {
	log.Printf("lifecycle: event asg=%s instance=%s transition=%s",
		ev.ASGName, ev.InstanceID, ev.LifecycleTransition)

	if ev.LifecycleTransition != "" && ev.LifecycleTransition != "autoscaling:EC2_INSTANCE_LAUNCHING" {
		// Terminating hooks aren't wired yet; ignore.
		log.Printf("lifecycle: decision instance=%s decision=ABANDON reason=ignored-transition transition=%s",
			ev.InstanceID, ev.LifecycleTransition)
		return DecisionAbandon, fmt.Errorf("ignored transition %q", ev.LifecycleTransition)
	}
	tier, ok := h.tierByASG[ev.ASGName]
	if !ok {
		log.Printf("lifecycle: decision instance=%s asg=%s decision=ABANDON reason=unknown-asg",
			ev.InstanceID, ev.ASGName)
		return DecisionAbandon, fmt.Errorf("unknown ASG %q (not a shard tier)", ev.ASGName)
	}

	az, err := h.lookupInstanceAZ(ctx, ev.InstanceID)
	if err != nil {
		log.Printf("lifecycle: decision instance=%s tier=%s decision=ABANDON reason=az-lookup-failed err=%v",
			ev.InstanceID, tier, err)
		return DecisionAbandon, fmt.Errorf("lookup AZ for %s: %w", ev.InstanceID, err)
	}

	decision, volumeID, shardID, err := h.decide(ctx, ev.InstanceID, tier, az)
	if err != nil {
		log.Printf("lifecycle: decision instance=%s tier=%s az=%s decision=%s reason=%s err=%v",
			ev.InstanceID, tier, az, decision, abandonReason(err), err)
		h.signal(ctx, ev, "ABANDON")
		return DecisionAbandon, err
	}

	if err := h.volumes.AttachToInstance(ctx, volumeID, ev.InstanceID, h.deviceName); err != nil {
		log.Printf("lifecycle: decision instance=%s tier=%s az=%s decision=ABANDON reason=attach-failed volume=%s err=%v",
			ev.InstanceID, tier, az, volumeID, err)
		h.signal(ctx, ev, "ABANDON")
		return DecisionAbandon, err
	}

	log.Printf("lifecycle: decision asg=%s instance=%s tier=%s az=%s decision=%s volume=%s shard=%s",
		ev.ASGName, ev.InstanceID, tier, az, decision, volumeID, shardID)
	h.signal(ctx, ev, "CONTINUE")
	return decision, nil
}

// abandonReason maps decide()'s error messages back to a stable
// short reason tag for log filtering. Adds no semantic info — exists
// purely so on-call greps land on a consistent key.
func abandonReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no LAUNCHING row"):
		return "no-launching-row"
	case strings.Contains(msg, "no data_volume_size_gb"):
		return "no-tier-size"
	case strings.Contains(msg, "no ShardOps store"):
		return "no-shardops-store"
	case strings.Contains(msg, "create volume"):
		return "create-volume-failed"
	case strings.Contains(msg, "claim LAUNCHING row"):
		return "claim-failed"
	default:
		return "unknown"
	}
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
// decide returns (decision, volumeID, shardID, err). shardID is the
// recovered shard's id on RECOVER, the freshly-claimed row's id on
// CHRISTEN, or empty on ABANDON.
func (h *LifecycleHandler) decide(ctx context.Context, instanceID, tier, az string) (LifecycleDecision, string, string, error) {
	candidates, err := h.volumes.ListAvailableByTier(ctx, tier, az)
	if err != nil {
		log.Printf("lifecycle: scan tier=%s az=%s instance=%s status=err err=%v",
			tier, az, instanceID, err)
		return DecisionAbandon, "", "", err
	}
	log.Printf("lifecycle: scan tier=%s az=%s instance=%s candidates=%d",
		tier, az, instanceID, len(candidates))

	// Filter to volumes whose shard id we actually recognize as
	// "live" in the registry/store. Untagged volumes (shard_id
	// absent) are christening leftovers and are not recovery
	// candidates; tagged-but-unknown ones are abandoned cruft
	// belonging to a shard we've already torn down.
	var recoverable []VolumeInfo
	for _, v := range candidates {
		if v.ShardID == "" {
			log.Printf("lifecycle: candidate volume=%s shard= status=skip reason=untagged",
				v.VolumeID)
			continue
		}
		if !h.recognisedAsDead(v.ShardID) {
			log.Printf("lifecycle: candidate volume=%s shard=%s status=filter reason=not-recognised-as-dead",
				v.VolumeID, v.ShardID)
			continue
		}
		log.Printf("lifecycle: candidate volume=%s shard=%s status=accept",
			v.VolumeID, v.ShardID)
		recoverable = append(recoverable, v)
	}
	log.Printf("lifecycle: scan tier=%s instance=%s recoverable=%d",
		tier, instanceID, len(recoverable))

	switch {
	case len(recoverable) > 0:
		return DecisionRecover, recoverable[0].VolumeID, recoverable[0].ShardID, nil
	default:
		size, ok := h.sizeByTier[tier]
		if !ok || size <= 0 {
			return DecisionAbandon, "", "", fmt.Errorf("no data_volume_size_gb configured for tier %q", tier)
		}
		// CHRISTEN requires a reconciler-minted LAUNCHING row
		// naming this tier. The row becomes the volume's shard_id
		// tag — no placeholder, no re-tag post-boot. An instance
		// reaching this branch without a row is an orphan (ASG
		// scaled out-of-band, or reconciler/ASG state diverged):
		// ABANDON so the ASG terminates it.
		if h.shardOps == nil {
			return DecisionAbandon, "", "", fmt.Errorf("christen for %s: no ShardOps store — can't claim LAUNCHING row", instanceID)
		}
		claimed, claimOK, err := h.shardOps.ClaimLaunchingForTier(tier, instanceID)
		if err != nil {
			return DecisionAbandon, "", "", fmt.Errorf("claim LAUNCHING row for %s/%s: %w", tier, instanceID, err)
		}
		if !claimOK {
			return DecisionAbandon, "", "", fmt.Errorf("no LAUNCHING row for tier %s; instance %s is an orphan", tier, instanceID)
		}
		log.Printf("lifecycle: claim tier=%s instance=%s row=%s status=ok",
			tier, instanceID, claimed.ID)
		id, err := h.volumes.CreateForShard(ctx, CreateVolumeOpts{
			ShardID: claimed.ID,
			Tier:    tier,
			AZ:      az,
			SizeGB:  size,
		})
		if err != nil {
			return DecisionAbandon, "", "", fmt.Errorf("create volume for new shard: %w", err)
		}
		return DecisionChristen, id, claimed.ID, nil
	}
}

// recognisedAsDead returns true when the given shard id names a shard
// the coordinator still considers hot (in the registry) AND whose
// last instance's heartbeat is stale enough that we're comfortable
// yanking the volume out from under it.
//
// Logs the per-shard determination at every call so refresh-induced
// ABANDON traces in prod can be reconstructed: each filtered volume
// is matched up with the reason its shard didn't qualify (registry
// miss, archived state, live heartbeat within deadFor).
func (h *LifecycleHandler) recognisedAsDead(shardID string) bool {
	if h.registry == nil {
		log.Printf("lifecycle: recognised-as-dead shard=%s result=false reason=no-registry",
			shardID)
		return false
	}
	entry := h.registry.Get(shardID)
	if entry == nil {
		log.Printf("lifecycle: recognised-as-dead shard=%s result=false reason=not-in-registry",
			shardID)
		return false
	}
	if entry.State == StateArchived || entry.State == StateExpiring {
		log.Printf("lifecycle: recognised-as-dead shard=%s result=false reason=state state=%s",
			shardID, entry.State)
		return false
	}
	// h.store is enforced non-nil by NewLifecycleHandler; no defensive
	// branch here. A nil store is a construction-time invariant
	// violation, not a runtime decision.
	heartbeats, err := h.store.LiveShardHeartbeats(fmt.Sprintf("%d seconds", int(h.deadFor.Seconds())))
	if err != nil {
		log.Printf("lifecycle: recognised-as-dead shard=%s result=true reason=store-err err=%v",
			shardID, err)
		return true // we'd rather recover than leave a shard stranded on a store hiccup
	}
	for _, hb := range heartbeats {
		if hb.ShardID == shardID {
			log.Printf("lifecycle: recognised-as-dead shard=%s result=false reason=heartbeat-live deadFor=%s",
				shardID, h.deadFor)
			return false
		}
	}
	log.Printf("lifecycle: recognised-as-dead shard=%s result=true reason=no-recent-heartbeat deadFor=%s",
		shardID, h.deadFor)
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
