// Package nat is hosted weft's NAT route election: of the coordinators running,
// one at a time points the private subnet's default route at its own ENI. It
// exists because hosted shares one NAT instance among its coordinators instead
// of paying for a NAT gateway; nothing outside tenancy/hosted knows about it.
package nat

import (
	"context"
	"fmt"
	"os"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/graybeardcorp/weft/app"
	"github.com/graybeardcorp/weft/coordination"
)

// natLeaseName is the coordination.Leases name guarding which coordinator
// instance owns the private subnet's 0.0.0.0/0 NAT route. See
// docs/plans/coordinator-horizontal-scaling.md #1.
const natLeaseName = "nat-holder"

// EC2Client is the narrow EC2 surface Elector needs — narrow so
// tests don't have to stand up a whole EC2 client.
type EC2Client interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	ReplaceRoute(ctx context.Context, in *ec2.ReplaceRouteInput, opts ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error)
	CreateRoute(ctx context.Context, in *ec2.CreateRouteInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error)
}

// Elector runs the coordinator side of NAT self-election: exactly one
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
type Elector struct {
	leases       coordination.Leases
	ec2          EC2Client
	routeTableID string
	eniID        string
	holder       string
	done         chan struct{}
}

// Start is the hosted coordinator's NAT election, from what the running
// process gives it. It does nothing off EC2 (no instance id) or when
// NAT_ROUTE_TABLE_ID is unset, the expected shape in dev and docker. It
// registers the election's end with the process, so a stopped coordinator
// gives the lease back before it exits.
func Start(rt app.Runtime) {
	routeTableID := os.Getenv("NAT_ROUTE_TABLE_ID")
	if rt.InstanceID == "" || routeTableID == "" {
		return
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(rt.AWSRegion))
	if err != nil {
		weftsdk.Errorf(rt.Ctx, "nat: aws config load failed, not running NAT election: %v", err)
		return
	}
	ec2Client := ec2.NewFromConfig(awsCfg)
	eniID, err := ResolveOwnENI(context.Background(), ec2Client, rt.InstanceID)
	if err != nil {
		weftsdk.Errorf(rt.Ctx, "nat: resolve own ENI failed, not running NAT election: %v", err)
		return
	}
	e := New(rt.Leases, ec2Client, routeTableID, eniID, rt.Holder)
	e.Start(rt.Ctx)
	rt.AwaitOnExit(e.Done())
}

// Done is closed once the election loop has stopped and given the lease back.
// It is already closed when no loop runs.
func (e *Elector) Done() <-chan struct{} {
	if e.done == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return e.done
}

// New builds an Elector. routeTableID and eniID must both be
// non-empty for Start to actually run the election loop — see Start.
func New(l coordination.Leases, ec2Client EC2Client, routeTableID, eniID, holder string) *Elector {
	return &Elector{
		leases:       l,
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
func (e *Elector) Start(ctx context.Context) {
	if e.routeTableID == "" || e.eniID == "" {
		weftsdk.Infof(ctx, "nat: route table id or own ENI id unknown — not running NAT election (expected off-EC2)")
		return
	}
	e.done = make(chan struct{})
	go e.run(ctx)
}

func (e *Elector) run(ctx context.Context) {
	defer close(e.done)
	var amHolder bool
	e.tick(ctx, &amHolder)

	ticker := time.NewTicker(coordination.LeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if amHolder {
				// Best-effort — an unreleased lease just expires on its
				// own (LeaseTTL later), so skipping this on a crash is
				// never a correctness problem, only a slower handoff.
				if err := e.leases.ReleaseLease(natLeaseName, e.holder); err != nil {
					weftsdk.Infof(ctx, "nat: release lease on shutdown: %v", err)
				} else {
					coordination.LogLeaseEvent(natLeaseName, e.holder, "released")
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
func (e *Elector) tick(ctx context.Context, amHolder *bool) {
	ok, err := e.leases.AcquireLease(natLeaseName, e.holder, coordination.LeaseTTL)
	if err != nil {
		weftsdk.Errorf(ctx, "nat: lease acquire error: %v", err)
		return
	}
	switch {
	case ok && !*amHolder:
		weftsdk.Infof(ctx, "nat: acquired nat-holder lease, claiming route table %s → %s", e.routeTableID, e.eniID)
		coordination.LogLeaseEvent(natLeaseName, e.holder, "acquired")
		if err := e.claimRoute(ctx); err != nil {
			weftsdk.Errorf(ctx, "nat: claim route failed, will retry next tick: %v", err)
			return
		}
		*amHolder = true
	case !ok && *amHolder:
		weftsdk.Infof(ctx, "nat: lost nat-holder lease")
		coordination.LogLeaseEvent(natLeaseName, e.holder, "lost")
		*amHolder = false
	}
}

func (e *Elector) claimRoute(ctx context.Context) error {
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
func ResolveOwnENI(ctx context.Context, ec2Client EC2Client, instanceID string) (string, error) {
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
