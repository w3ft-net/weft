package coordinator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/graybeardcorp/weft/store"
)

// natLeaseName is the store.AcquireLease name guarding which coordinator
// instance owns the private subnet's 0.0.0.0/0 NAT route. See
// docs/plans/coordinator-horizontal-scaling.md #1.
const natLeaseName = "nat-holder"

// natEC2Client is the narrow EC2 surface NATElector needs — narrow so
// tests don't have to stand up a whole EC2 client.
type natEC2Client interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	ReplaceRoute(ctx context.Context, in *ec2.ReplaceRouteInput, opts ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error)
	CreateRoute(ctx context.Context, in *ec2.CreateRouteInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error)
}

// NATElector runs the coordinator side of NAT self-election: exactly one
// coordinator instance holds the "nat-holder" lease at a time, and only
// the holder points the private subnet's route table at its own ENI.
//
// Replaces the old boot-time-only claim in
// infrastructure-catalog/modules/asg-application/user-data.sh.tpl, which
// raced on boot order — whichever coordinator instance booted (or
// rebooted) last silently won the route, with no ongoing reconciliation.
// If that instance later terminated, every private-subnet instance lost
// egress until some other coordinator happened to reboot. See
// docs/plans/coordinator-horizontal-scaling.md #1.
//
// AcquireLease's self-renewal bias (WHERE holder = $self OR expires_at <
// now(), store/leases.go) already gives a healthy incumbent priority over
// challengers: a challenger only wins once the incumbent has missed
// LeaseTTL worth of renewals, not on ordinary tick jitter or a routine
// fleet-cycling deploy (the new instance can't out-race a still-healthy
// old one). So there's no separate anti-flap logic here — the lease
// primitive already provides it.
type NATElector struct {
	store        *store.Store
	ec2          natEC2Client
	routeTableID string
	eniID        string
	holder       string
}

// NewNATElector builds a NATElector. routeTableID and eniID must both be
// non-empty for Start to actually run the election loop — see Start.
func NewNATElector(s *store.Store, ec2Client natEC2Client, routeTableID, eniID, holder string) *NATElector {
	return &NATElector{
		store:        s,
		ec2:          ec2Client,
		routeTableID: routeTableID,
		eniID:        eniID,
		holder:       holder,
	}
}

// Start runs the election loop in a background goroutine until ctx is
// canceled. No-ops (logging why) if routeTableID or eniID is empty —
// the expected shape off-EC2 (local/docker dev), where there's no real
// NAT route table to claim.
func (e *NATElector) Start(ctx context.Context) {
	if e.routeTableID == "" || e.eniID == "" {
		log.Printf("nat: route table id or own ENI id unknown — not running NAT election (expected off-EC2)")
		return
	}
	go e.run(ctx)
}

func (e *NATElector) run(ctx context.Context) {
	var amHolder bool
	e.tick(ctx, &amHolder)

	ticker := time.NewTicker(store.LeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if amHolder {
				// Best-effort — an unreleased lease just expires on its
				// own (LeaseTTL later), so skipping this on a crash is
				// never a correctness problem, only a slower handoff.
				if err := e.store.ReleaseLease(natLeaseName, e.holder); err != nil {
					log.Printf("nat: release lease on shutdown: %v", err)
				} else {
					store.LogLeaseEvent(natLeaseName, e.holder, "released")
				}
			}
			return
		case <-ticker.C:
			e.tick(ctx, &amHolder)
		}
	}
}

// tick attempts to acquire/renew the lease and, only on the transition
// from not-holding to holding, claims the route. A transient AcquireLease
// error leaves *amHolder unchanged: unlike AcquireAndHold's guarded
// in-process work (which fails closed because two instances both
// believing they hold the same lease would be actively harmful), the
// thing tick guards here is external AWS route state — safe to leave
// alone and retry next tick rather than react to a single failed DB call.
func (e *NATElector) tick(ctx context.Context, amHolder *bool) {
	ok, err := e.store.AcquireLease(natLeaseName, e.holder, store.LeaseTTL)
	if err != nil {
		log.Printf("nat: lease acquire error: %v", err)
		return
	}
	switch {
	case ok && !*amHolder:
		log.Printf("nat: acquired nat-holder lease, claiming route table %s → %s", e.routeTableID, e.eniID)
		store.LogLeaseEvent(natLeaseName, e.holder, "acquired")
		if err := e.claimRoute(ctx); err != nil {
			log.Printf("nat: claim route failed, will retry next tick: %v", err)
			return
		}
		*amHolder = true
	case !ok && *amHolder:
		log.Printf("nat: lost nat-holder lease")
		store.LogLeaseEvent(natLeaseName, e.holder, "lost")
		*amHolder = false
	}
}

func (e *NATElector) claimRoute(ctx context.Context) error {
	_, err := e.ec2.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(e.routeTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NetworkInterfaceId:   aws.String(e.eniID),
	})
	if err == nil {
		return nil
	}
	// ReplaceRoute fails if the 0.0.0.0/0 route doesn't exist yet (fresh
	// route table) — fall back to creating it, matching the old bash
	// claim's replace-then-create fallback.
	if _, err := e.ec2.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(e.routeTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NetworkInterfaceId:   aws.String(e.eniID),
	}); err != nil {
		return fmt.Errorf("replace and create route both failed: %w", err)
	}
	return nil
}

// ResolveOwnENI looks up the primary ENI of the given instance, matching
// the lookup the old bash boot claim used
// (NetworkInterfaces[0].NetworkInterfaceId).
func ResolveOwnENI(ctx context.Context, ec2Client natEC2Client, instanceID string) (string, error) {
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", fmt.Errorf("describe instance %s: %w", instanceID, err)
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}
	nics := out.Reservations[0].Instances[0].NetworkInterfaces
	if len(nics) == 0 {
		return "", fmt.Errorf("instance %s has no network interfaces", instanceID)
	}
	return aws.ToString(nics[0].NetworkInterfaceId), nil
}
