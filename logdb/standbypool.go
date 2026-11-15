package logdb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
)

// The warm pool (docs/plans/shard-seamless-upgrade.md): one booted,
// volume-less shard instance per (tier, AZ) that some bucket or live shard
// uses, so an upgrade, a recovery, or a mint claims an instance that is
// already up instead of waiting minutes on EC2. The pool only ever shortens
// a path: every claimant falls back to an on-demand launch when no standby is
// free.

// standbyBootBudget is how long a standby may take to send its first heartbeat
// before the reconciler gives up on it and launches another (the pool's
// version of LaunchRetryBudget).
const standbyBootBudget = 10 * time.Minute

// StandbyPair is a (tier, AZ) the pool keeps a standby for.
type StandbyPair struct {
	Tier string
	AZ   string
}

// StandbyInstance is a pool instance as EC2 reports it. ClaimedBy is the
// shard_id tag a claim stamps, "" for an unclaimed standby.
type StandbyInstance struct {
	InstanceID string
	Tier       string
	AZ         string
	ImageID    string
	LaunchedAt time.Time
	ClaimedBy  string
}

// StandbyLauncher is the EC2 surface the pool needs. EC2Provisioner
// implements it. Every pool instance carries a shard_spare tag from birth
// until the instance becomes a shard's (ReleaseStandby), which is also what
// boots it in standby mode.
type StandbyLauncher interface {
	// LaunchStandby runs a standby for (tier, az) from the current image,
	// stamped as claimed by claimFor when that is non-empty (an on-demand
	// launch for one path).
	LaunchStandby(ctx context.Context, tier, az, claimFor string) (StandbyInstance, error)
	// ListStandbyInstances returns every pending or running instance still
	// tagged as a standby, claimed or not.
	ListStandbyInstances(ctx context.Context) ([]StandbyInstance, error)
	// ClaimInstance stamps the claiming shard on a standby.
	ClaimInstance(ctx context.Context, instanceID, shardID string) error
	// ReleaseStandby drops the standby tag once the instance is a shard's.
	ReleaseStandby(ctx context.Context, instanceID string) error
	Terminate(ctx context.Context, instanceID string) error
	// CurrentImageID is the AMI shards launch from now. Only a standby on it
	// is claimable.
	CurrentImageID() string
}

// StandbyUse is what a claimed standby is to the path that claimed it.
type StandbyUse int

const (
	// StandbyInUse: the path is still using it (an upgrade's successor, a
	// launch in flight). Leave it.
	StandbyInUse StandbyUse = iota
	// StandbyAdopted: the standby is now the shard's own instance. The pool
	// forgets it.
	StandbyAdopted
	// StandbyAbandoned: its path is gone or gave up. Terminate it; it may have
	// had a volume attached, so it never returns to the pool.
	StandbyAbandoned
)

// StandbyPool owns the pool's convergence and claims.
type StandbyPool struct {
	store StandbyStore
	ec2   StandbyLauncher
	clock func() time.Time
}

// NewStandbyPool builds a pool over the shard_spares table and EC2.
func NewStandbyPool(store StandbyStore, ec2 StandbyLauncher, clock func() time.Time) *StandbyPool {
	if clock == nil {
		clock = time.Now
	}
	return &StandbyPool{store: store, ec2: ec2, clock: clock}
}

// Reconcile drives the pool one step toward one heartbeating, current-image
// standby for every desired pair. It runs on the FSM leader every tick:
//
//  1. reconcile the table against EC2: adopt a tagged instance with no row,
//     forget an unclaimed row whose instance is gone;
//  2. settle claimed rows by asking use what each is to its path;
//  3. retire unclaimed standbys that never booted, or whose pair is no longer
//     desired;
//  4. per desired pair, launch a current-image standby if it has none, and
//     only once that one heartbeats terminate any stale-image or surplus
//     standby, so the pair is never uncovered.
//
// Each step logs and moves on past a failure; the next tick retries.
func (p *StandbyPool) Reconcile(ctx context.Context, desired map[StandbyPair]bool, use func(Standby) StandbyUse) error {
	rows, err := p.store.ListStandbys()
	if err != nil {
		return fmt.Errorf("list spares: %w", err)
	}
	insts, err := p.ec2.ListStandbyInstances(ctx)
	if err != nil {
		return fmt.Errorf("list spare instances: %w", err)
	}
	now := p.clock()

	// 1. Table vs EC2.
	inEC2 := make(map[string]StandbyInstance, len(insts))
	for _, in := range insts {
		inEC2[in.InstanceID] = in
	}
	inTable := make(map[string]bool, len(rows))
	live := rows[:0]
	for _, sp := range rows {
		inTable[sp.InstanceID] = true
		if _, ok := inEC2[sp.InstanceID]; !ok && !sp.Claimed() {
			p.forget(ctx, sp.InstanceID, "its instance is gone")
			continue
		}
		live = append(live, sp)
	}
	for _, in := range insts {
		if inTable[in.InstanceID] {
			continue
		}
		sp := Standby{InstanceID: in.InstanceID, Tier: in.Tier, AZ: in.AZ, ImageID: in.ImageID, LaunchedAt: in.LaunchedAt}
		if in.ClaimedBy != "" {
			claimant := in.ClaimedBy
			sp.ClaimedBy, sp.ClaimedAt = &claimant, &now
		}
		if err := p.store.InsertStandby(&sp); err != nil {
			weftsdk.Infof(ctx, "sparepool: adopt %s: %v", in.InstanceID, err)
			continue
		}
		weftsdk.Infof(ctx, "sparepool: adopted untracked spare %s (%s/%s, claimed by %q)", in.InstanceID, in.Tier, in.AZ, in.ClaimedBy)
		live = append(live, sp)
	}

	// 2 + 3. Claimed rows go to their path; unclaimed rows are checked for
	// boot and desirability.
	current := p.ec2.CurrentImageID()
	byPair := map[StandbyPair][]Standby{}
	for _, sp := range live {
		pair := StandbyPair{sp.Tier, sp.AZ}
		switch {
		case sp.Claimed():
			switch use(sp) {
			case StandbyAdopted:
				if err := p.ec2.ReleaseStandby(ctx, sp.InstanceID); err != nil {
					weftsdk.Infof(ctx, "sparepool: release spare tag on %s: %v", sp.InstanceID, err)
					continue
				}
				p.forget(ctx, sp.InstanceID, "it is now shard "+*sp.ClaimedBy+"'s instance")
			case StandbyAbandoned:
				p.retire(ctx, sp.InstanceID, "its claim by "+*sp.ClaimedBy+" was abandoned")
			}
		case sp.LastHeartbeatAt == nil && now.Sub(sp.LaunchedAt) > standbyBootBudget:
			p.retire(ctx, sp.InstanceID, fmt.Sprintf("no heartbeat within %s of launch", standbyBootBudget))
		case !desired[pair]:
			p.retire(ctx, sp.InstanceID, fmt.Sprintf("no bucket or shard uses %s/%s", sp.Tier, sp.AZ))
		default:
			byPair[pair] = append(byPair[pair], sp)
		}
	}

	// 4. Cover every desired pair, new first.
	pairs := make([]StandbyPair, 0, len(desired))
	for pair, want := range desired {
		if want {
			pairs = append(pairs, pair)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Tier != pairs[j].Tier {
			return pairs[i].Tier < pairs[j].Tier
		}
		return pairs[i].AZ < pairs[j].AZ
	})
	for _, pair := range pairs {
		var fresh, stale []Standby
		for _, sp := range byPair[pair] {
			if sp.ImageID == current {
				fresh = append(fresh, sp)
			} else {
				stale = append(stale, sp)
			}
		}
		if len(fresh) == 0 {
			if _, err := p.launch(ctx, pair, ""); err != nil {
				weftsdk.Errorf(ctx, "sparepool: launch spare for %s/%s: %v", pair.Tier, pair.AZ, err)
			}
			continue
		}
		// Keep the heartbeating fresh standby (the oldest, if several); the
		// rest of the fresh ones are surplus.
		sort.SliceStable(fresh, func(i, j int) bool {
			bi, bj := fresh[i].LastHeartbeatAt != nil, fresh[j].LastHeartbeatAt != nil
			if bi != bj {
				return bi
			}
			return fresh[i].LaunchedAt.Before(fresh[j].LaunchedAt)
		})
		if fresh[0].LastHeartbeatAt == nil {
			continue // its replacement-in-waiting is still booting; the stale one keeps covering
		}
		for _, sp := range fresh[1:] {
			p.retire(ctx, sp.InstanceID, "surplus spare for "+pair.Tier+"/"+pair.AZ)
		}
		for _, sp := range stale {
			p.retire(ctx, sp.InstanceID, fmt.Sprintf("image %s replaced by %s", sp.ImageID, current))
		}
	}
	return nil
}

// launch runs a standby for pair and records it; claimFor non-empty records
// it as already claimed (an on-demand launch).
func (p *StandbyPool) launch(ctx context.Context, pair StandbyPair, claimFor string) (*Standby, error) {
	in, err := p.ec2.LaunchStandby(ctx, pair.Tier, pair.AZ, claimFor)
	if err != nil {
		return nil, err
	}
	sp := &Standby{InstanceID: in.InstanceID, Tier: pair.Tier, AZ: pair.AZ, ImageID: in.ImageID, LaunchedAt: p.clock()}
	if claimFor != "" {
		at := p.clock()
		sp.ClaimedBy, sp.ClaimedAt = &claimFor, &at
	}
	if err := p.store.InsertStandby(sp); err != nil {
		// The instance carries its tags; the next Reconcile adopts it.
		return nil, fmt.Errorf("record spare %s: %w", in.InstanceID, err)
	}
	weftsdk.Infof(ctx, "sparepool: launched spare %s for %s/%s (image %s, claimed by %q)", in.InstanceID, pair.Tier, pair.AZ, in.ImageID, claimFor)
	return sp, nil
}

// retire terminates a standby and forgets it. A failed terminate keeps the
// row so the next tick retries.
func (p *StandbyPool) retire(ctx context.Context, instanceID, why string) {
	if err := p.ec2.Terminate(ctx, instanceID); err != nil {
		weftsdk.Infof(ctx, "sparepool: terminate %s (%s): %v", instanceID, why, err)
		return
	}
	p.forget(ctx, instanceID, why+"; terminated")
}

func (p *StandbyPool) forget(ctx context.Context, instanceID, why string) {
	if err := p.store.DeleteStandby(instanceID); err != nil {
		weftsdk.Infof(ctx, "sparepool: forget %s: %v", instanceID, err)
		return
	}
	weftsdk.Infof(ctx, "sparepool: dropped spare %s: %s", instanceID, why)
}

// Claim takes a current-image standby in (tier, az) for shardID, or returns
// nil when the pool has none (the caller launches on demand). Idempotent:
// a standby shardID already holds is returned again. An old-image standby is
// never claimed, on any path: landing a shard on it would leave the shard
// for the roll to upgrade again.
func (p *StandbyPool) Claim(ctx context.Context, shardID, tier, az string) (*Standby, error) {
	sp, err := p.store.ClaimStandby(shardID, tier, az, p.ec2.CurrentImageID(), p.clock())
	if err != nil || sp == nil {
		return nil, err
	}
	if err := p.ec2.ClaimInstance(ctx, sp.InstanceID, shardID); err != nil {
		return nil, fmt.Errorf("stamp claim on %s: %w", sp.InstanceID, err)
	}
	weftsdk.Infof(ctx, "sparepool: %s claimed spare %s (%s/%s)", shardID, sp.InstanceID, tier, az)
	return sp, nil
}

// ClaimOrLaunch is Claim with the on-demand fallback: when the pool has
// nothing for (tier, az), launch a standby already claimed by shardID. Either
// way the caller gets the standby it holds.
func (p *StandbyPool) ClaimOrLaunch(ctx context.Context, shardID, tier, az string) (*Standby, error) {
	sp, err := p.Claim(ctx, shardID, tier, az)
	if err != nil || sp != nil {
		return sp, err
	}
	weftsdk.Infof(ctx, "sparepool: no current-image spare for %s/%s; launching one on demand for %s", tier, az, shardID)
	return p.launch(ctx, StandbyPair{tier, az}, shardID)
}

// Held returns the standby shardID holds, with its latest heartbeat facts.
func (p *StandbyPool) Held(shardID string) (*Standby, error) {
	return p.store.StandbyClaimedBy(shardID)
}

// Adopt marks a claimed standby as the shard's own instance: the standby tag
// goes and the pool forgets it.
func (p *StandbyPool) Adopt(ctx context.Context, instanceID string) error {
	if err := p.ec2.ReleaseStandby(ctx, instanceID); err != nil {
		return err
	}
	return p.store.DeleteStandby(instanceID)
}

// Abandon terminates a claimed standby its path gave up on and forgets it.
func (p *StandbyPool) Abandon(ctx context.Context, instanceID string) error {
	if err := p.ec2.Terminate(ctx, instanceID); err != nil {
		return err
	}
	return p.store.DeleteStandby(instanceID)
}

// Covered reports whether (tier, az) has an unclaimed, heartbeating standby
// on the current image: an upgrade's demoted instance waits for this before
// it is terminated. The standby the upgrade claimed no longer counts, so this
// is its replacement, which the reconciler launches as soon as the claim
// leaves the pair bare (usually while the swap is still running).
func (p *StandbyPool) Covered(tier, az string) (bool, error) {
	rows, err := p.store.ListStandbys()
	if err != nil {
		return false, err
	}
	current := p.ec2.CurrentImageID()
	for _, sp := range rows {
		if !sp.Claimed() && sp.Tier == tier && sp.AZ == az && sp.ImageID == current && sp.LastHeartbeatAt != nil {
			return true, nil
		}
	}
	return false, nil
}

// ObserveHeartbeat records a standby's heartbeat on its row.
func (p *StandbyPool) ObserveHeartbeat(instanceID, addr, binarySHA string) error {
	if instanceID == "" {
		return errors.New("spare heartbeat without an instance id")
	}
	return p.store.ObserveStandbyHeartbeat(instanceID, addr, binarySHA, p.clock())
}
