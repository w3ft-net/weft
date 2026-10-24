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
// a path: every claimant falls back to an on-demand launch when no spare is
// free.

// spareBootBudget is how long a spare may take to send its first heartbeat
// before the reconciler gives up on it and launches another (the pool's
// version of LaunchRetryBudget).
const spareBootBudget = 10 * time.Minute

// SparePair is a (tier, AZ) the pool keeps a spare for.
type SparePair struct {
	Tier string
	AZ   string
}

// SpareInstance is a pool instance as EC2 reports it. ClaimedBy is the
// shard_id tag a claim stamps, "" for an unclaimed spare.
type SpareInstance struct {
	InstanceID string
	Tier       string
	AZ         string
	ImageID    string
	LaunchedAt time.Time
	ClaimedBy  string
}

// SpareLauncher is the EC2 surface the pool needs. EC2Provisioner
// implements it. Every pool instance carries a shard_spare tag from birth
// until the instance becomes a shard's (ReleaseSpare), which is also what
// boots it in spare mode.
type SpareLauncher interface {
	// LaunchSpare runs a spare for (tier, az) from the current image,
	// stamped as claimed by claimFor when that is non-empty (an on-demand
	// launch for one path).
	LaunchSpare(ctx context.Context, tier, az, claimFor string) (SpareInstance, error)
	// ListSpareInstances returns every pending or running instance still
	// tagged as a spare, claimed or not.
	ListSpareInstances(ctx context.Context) ([]SpareInstance, error)
	// ClaimInstance stamps the claiming shard on a spare.
	ClaimInstance(ctx context.Context, instanceID, shardID string) error
	// ReleaseSpare drops the spare tag once the instance is a shard's.
	ReleaseSpare(ctx context.Context, instanceID string) error
	Terminate(ctx context.Context, instanceID string) error
	// CurrentImageID is the AMI shards launch from now. Only a spare on it
	// is claimable.
	CurrentImageID() string
}

// SpareUse is what a claimed spare is to the path that claimed it.
type SpareUse int

const (
	// SpareInUse: the path is still using it (an upgrade's successor, a
	// launch in flight). Leave it.
	SpareInUse SpareUse = iota
	// SpareAdopted: the spare is now the shard's own instance. The pool
	// forgets it.
	SpareAdopted
	// SpareAbandoned: its path is gone or gave up. Terminate it; it may have
	// had a volume attached, so it never returns to the pool.
	SpareAbandoned
)

// SparePool owns the pool's convergence and claims.
type SparePool struct {
	store SpareStore
	ec2   SpareLauncher
	clock func() time.Time
}

// NewSparePool builds a pool over the shard_spares table and EC2.
func NewSparePool(store SpareStore, ec2 SpareLauncher, clock func() time.Time) *SparePool {
	if clock == nil {
		clock = time.Now
	}
	return &SparePool{store: store, ec2: ec2, clock: clock}
}

// Reconcile drives the pool one step toward one heartbeating, current-image
// spare for every desired pair. It runs on the FSM leader every tick:
//
//  1. reconcile the table against EC2: adopt a tagged instance with no row,
//     forget an unclaimed row whose instance is gone;
//  2. settle claimed rows by asking use what each is to its path;
//  3. retire unclaimed spares that never booted, or whose pair is no longer
//     desired;
//  4. per desired pair, launch a current-image spare if it has none, and
//     only once that one heartbeats terminate any stale-image or surplus
//     spare, so the pair is never uncovered.
//
// Each step logs and moves on past a failure; the next tick retries.
func (p *SparePool) Reconcile(ctx context.Context, desired map[SparePair]bool, use func(Spare) SpareUse) error {
	rows, err := p.store.ListSpares()
	if err != nil {
		return fmt.Errorf("list spares: %w", err)
	}
	insts, err := p.ec2.ListSpareInstances(ctx)
	if err != nil {
		return fmt.Errorf("list spare instances: %w", err)
	}
	now := p.clock()

	// 1. Table vs EC2.
	inEC2 := make(map[string]SpareInstance, len(insts))
	for _, in := range insts {
		inEC2[in.InstanceID] = in
	}
	inTable := make(map[string]bool, len(rows))
	live := rows[:0]
	for _, sp := range rows {
		inTable[sp.InstanceID] = true
		if _, ok := inEC2[sp.InstanceID]; !ok && !sp.claimed() {
			p.forget(ctx, sp.InstanceID, "its instance is gone")
			continue
		}
		live = append(live, sp)
	}
	for _, in := range insts {
		if inTable[in.InstanceID] {
			continue
		}
		sp := Spare{InstanceID: in.InstanceID, Tier: in.Tier, AZ: in.AZ, ImageID: in.ImageID, LaunchedAt: in.LaunchedAt}
		if in.ClaimedBy != "" {
			claimant := in.ClaimedBy
			sp.ClaimedBy, sp.ClaimedAt = &claimant, &now
		}
		if err := p.store.InsertSpare(&sp); err != nil {
			weftsdk.Infof(ctx, "sparepool: adopt %s: %v", in.InstanceID, err)
			continue
		}
		weftsdk.Infof(ctx, "sparepool: adopted untracked spare %s (%s/%s, claimed by %q)", in.InstanceID, in.Tier, in.AZ, in.ClaimedBy)
		live = append(live, sp)
	}

	// 2 + 3. Claimed rows go to their path; unclaimed rows are checked for
	// boot and desirability.
	current := p.ec2.CurrentImageID()
	byPair := map[SparePair][]Spare{}
	for _, sp := range live {
		pair := SparePair{sp.Tier, sp.AZ}
		switch {
		case sp.claimed():
			switch use(sp) {
			case SpareAdopted:
				if err := p.ec2.ReleaseSpare(ctx, sp.InstanceID); err != nil {
					weftsdk.Infof(ctx, "sparepool: release spare tag on %s: %v", sp.InstanceID, err)
					continue
				}
				p.forget(ctx, sp.InstanceID, "it is now shard "+*sp.ClaimedBy+"'s instance")
			case SpareAbandoned:
				p.retire(ctx, sp.InstanceID, "its claim by "+*sp.ClaimedBy+" was abandoned")
			}
		case sp.LastHeartbeatAt == nil && now.Sub(sp.LaunchedAt) > spareBootBudget:
			p.retire(ctx, sp.InstanceID, fmt.Sprintf("no heartbeat within %s of launch", spareBootBudget))
		case !desired[pair]:
			p.retire(ctx, sp.InstanceID, fmt.Sprintf("no bucket or shard uses %s/%s", sp.Tier, sp.AZ))
		default:
			byPair[pair] = append(byPair[pair], sp)
		}
	}

	// 4. Cover every desired pair, new first.
	pairs := make([]SparePair, 0, len(desired))
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
		var fresh, stale []Spare
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
		// Keep the heartbeating fresh spare (the oldest, if several); the
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

// launch runs a spare for pair and records it; claimFor non-empty records
// it as already claimed (an on-demand launch).
func (p *SparePool) launch(ctx context.Context, pair SparePair, claimFor string) (*Spare, error) {
	in, err := p.ec2.LaunchSpare(ctx, pair.Tier, pair.AZ, claimFor)
	if err != nil {
		return nil, err
	}
	sp := &Spare{InstanceID: in.InstanceID, Tier: pair.Tier, AZ: pair.AZ, ImageID: in.ImageID, LaunchedAt: p.clock()}
	if claimFor != "" {
		at := p.clock()
		sp.ClaimedBy, sp.ClaimedAt = &claimFor, &at
	}
	if err := p.store.InsertSpare(sp); err != nil {
		// The instance carries its tags; the next Reconcile adopts it.
		return nil, fmt.Errorf("record spare %s: %w", in.InstanceID, err)
	}
	weftsdk.Infof(ctx, "sparepool: launched spare %s for %s/%s (image %s, claimed by %q)", in.InstanceID, pair.Tier, pair.AZ, in.ImageID, claimFor)
	return sp, nil
}

// retire terminates a spare and forgets it. A failed terminate keeps the
// row so the next tick retries.
func (p *SparePool) retire(ctx context.Context, instanceID, why string) {
	if err := p.ec2.Terminate(ctx, instanceID); err != nil {
		weftsdk.Infof(ctx, "sparepool: terminate %s (%s): %v", instanceID, why, err)
		return
	}
	p.forget(ctx, instanceID, why+"; terminated")
}

func (p *SparePool) forget(ctx context.Context, instanceID, why string) {
	if err := p.store.DeleteSpare(instanceID); err != nil {
		weftsdk.Infof(ctx, "sparepool: forget %s: %v", instanceID, err)
		return
	}
	weftsdk.Infof(ctx, "sparepool: dropped spare %s: %s", instanceID, why)
}

// Claim takes a current-image spare in (tier, az) for shardID, or returns
// nil when the pool has none (the caller launches on demand). Idempotent:
// a spare shardID already holds is returned again. An old-image spare is
// never claimed, on any path: landing a shard on it would leave the shard
// for the roll to upgrade again.
func (p *SparePool) Claim(ctx context.Context, shardID, tier, az string) (*Spare, error) {
	sp, err := p.store.ClaimSpare(shardID, tier, az, p.ec2.CurrentImageID(), p.clock())
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
// nothing for (tier, az), launch a spare already claimed by shardID. Either
// way the caller gets the spare it holds.
func (p *SparePool) ClaimOrLaunch(ctx context.Context, shardID, tier, az string) (*Spare, error) {
	sp, err := p.Claim(ctx, shardID, tier, az)
	if err != nil || sp != nil {
		return sp, err
	}
	weftsdk.Infof(ctx, "sparepool: no current-image spare for %s/%s; launching one on demand for %s", tier, az, shardID)
	return p.launch(ctx, SparePair{tier, az}, shardID)
}

// Held returns the spare shardID holds, with its latest heartbeat facts.
func (p *SparePool) Held(shardID string) (*Spare, error) {
	return p.store.SpareClaimedBy(shardID)
}

// Adopt marks a claimed spare as the shard's own instance: the spare tag
// goes and the pool forgets it.
func (p *SparePool) Adopt(ctx context.Context, instanceID string) error {
	if err := p.ec2.ReleaseSpare(ctx, instanceID); err != nil {
		return err
	}
	return p.store.DeleteSpare(instanceID)
}

// Abandon terminates a claimed spare its path gave up on and forgets it.
func (p *SparePool) Abandon(ctx context.Context, instanceID string) error {
	if err := p.ec2.Terminate(ctx, instanceID); err != nil {
		return err
	}
	return p.store.DeleteSpare(instanceID)
}

// Covered reports whether (tier, az) has an unclaimed, heartbeating spare
// on the current image: an upgrade's demoted instance waits for this before
// it is terminated. The spare the upgrade claimed no longer counts, so this
// is its replacement, which the reconciler launches as soon as the claim
// leaves the pair bare (usually while the swap is still running).
func (p *SparePool) Covered(tier, az string) (bool, error) {
	rows, err := p.store.ListSpares()
	if err != nil {
		return false, err
	}
	current := p.ec2.CurrentImageID()
	for _, sp := range rows {
		if !sp.claimed() && sp.Tier == tier && sp.AZ == az && sp.ImageID == current && sp.LastHeartbeatAt != nil {
			return true, nil
		}
	}
	return false, nil
}

// ObserveHeartbeat records a spare's heartbeat on its row.
func (p *SparePool) ObserveHeartbeat(instanceID, addr, binarySHA string) error {
	if instanceID == "" {
		return errors.New("spare heartbeat without an instance id")
	}
	return p.store.ObserveSpareHeartbeat(instanceID, addr, binarySHA, p.clock())
}
