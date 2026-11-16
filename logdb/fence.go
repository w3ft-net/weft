package logdb

import (
	"context"
	"errors"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// errLostLease is what a leader's step returns once the shard-fsm-leader lease
// is gone: the new leader owns the shard, and anything this process still does
// to EC2 or a shard races it.
var errLostLease = errors.New("the shard-fsm-leader lease is gone")

// Shard.Evaluate checks leadership once, when it starts, and an upgrade then
// drives EC2 and the shard for tens of seconds. A process that loses the lease
// in the middle (it was stopped, or could not renew) must take no further step.
// Evaluate therefore marks its context with the lease check
// (ShardDeps.Leading), and every external call below refuses a context whose
// lease has gone.
//
// Only Evaluate marks it. A request that changes a shard (roll-shard's
// startUpgrade) arrives on whichever coordinator received it, follower
// included, and carries no mark.
//
// Not fenced, because neither takes a context to read the mark from: the
// export-output deleter (idempotent cleanup of an attempt's own partial
// output) and the standby pool's row store (a claim keyed by shard id, which
// returns the standby the shard already holds). A call already in flight when the
// lease changes hands can still land; only EC2-side conditions could stop it.
type leaderFenceKey struct{}

func withLeaderFence(ctx context.Context, leading func() bool) context.Context {
	return context.WithValue(ctx, leaderFenceKey{}, leading)
}

// leaseGone is errLostLease when ctx belongs to a leader's Evaluate whose
// lease has gone, nil otherwise.
func leaseGone(ctx context.Context) error {
	if leading, ok := ctx.Value(leaderFenceKey{}).(func() bool); ok && !leading() {
		return errLostLease
	}
	return nil
}

// fenceDeps wraps every external dependency of the shard FSM that takes a
// context. A nil dependency stays nil: callers test for that.
func fenceDeps(d *ShardDeps) {
	if d.Launcher != nil {
		d.Launcher = fencedLauncher{d.Launcher}
	}
	if d.Volumes != nil {
		d.Volumes = fencedVolumes{d.Volumes}
	}
	if d.Configurer != nil {
		d.Configurer = fencedConfigurer{d.Configurer}
	}
	if d.Exports != nil {
		d.Exports = fencedExports{d.Exports}
	}
	if d.Standbys != nil {
		d.Standbys = d.Standbys.fenced()
	}
}

// Each wrapper holds its interface in a named field, not an embedded one, and
// defines every method of it: a method added to the interface does not compile
// until it is fenced or deliberately passed through here.

type fencedLauncher struct{ inner ShardLaunchAPI }

func (f fencedLauncher) Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error) {
	if err := leaseGone(ctx); err != nil {
		return LaunchResult{}, err
	}
	return f.inner.Launch(ctx, spec)
}

func (f fencedLauncher) Terminate(ctx context.Context, instanceID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.Terminate(ctx, instanceID)
}

type fencedVolumes struct{ inner VolumeManager }

func (f fencedVolumes) CreateForShard(ctx context.Context, opts CreateVolumeOpts) (string, error) {
	if err := leaseGone(ctx); err != nil {
		return "", err
	}
	return f.inner.CreateForShard(ctx, opts)
}

func (f fencedVolumes) FindByShard(ctx context.Context, shardID string) (*VolumeInfo, error) {
	return f.inner.FindByShard(ctx, shardID)
}

func (f fencedVolumes) ListAvailableByTier(ctx context.Context, tier, az string) ([]VolumeInfo, error) {
	return f.inner.ListAvailableByTier(ctx, tier, az)
}

func (f fencedVolumes) TagWithShardID(ctx context.Context, volumeID, shardID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.TagWithShardID(ctx, volumeID, shardID)
}

func (f fencedVolumes) AttachToInstance(ctx context.Context, volumeID, instanceID, device string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.AttachToInstance(ctx, volumeID, instanceID, device)
}

func (f fencedVolumes) DetachFromInstance(ctx context.Context, volumeID string, force bool) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.DetachFromInstance(ctx, volumeID, force)
}

func (f fencedVolumes) DeleteVolume(ctx context.Context, volumeID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.DeleteVolume(ctx, volumeID)
}

type fencedPauser struct{ inner ShardPauser }

func (f fencedPauser) Pause(ctx context.Context, addr, successor string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.Pause(ctx, addr, successor)
}

func (f fencedPauser) Resume(ctx context.Context, addr string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.Resume(ctx, addr)
}

type fencedConfigurer struct{ inner ShardConfigurer }

func (f fencedConfigurer) Dial(ctx context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error) {
	if err := leaseGone(ctx); err != nil {
		return nil, err
	}
	return f.inner.Dial(ctx, addr, req)
}

type fencedExports struct{ inner ExportOps }

func (f fencedExports) CreateSnapshot(ctx context.Context, spec SnapshotSpec) (string, error) {
	if err := leaseGone(ctx); err != nil {
		return "", err
	}
	return f.inner.CreateSnapshot(ctx, spec)
}

func (f fencedExports) SnapshotState(ctx context.Context, snapshotID string) (ExportSnapshotState, error) {
	return f.inner.SnapshotState(ctx, snapshotID)
}

func (f fencedExports) LaunchInstance(ctx context.Context, spec ExportSpec) (string, error) {
	if err := leaseGone(ctx); err != nil {
		return "", err
	}
	return f.inner.LaunchInstance(ctx, spec)
}

func (f fencedExports) InstanceState(ctx context.Context, instanceID string) (string, error) {
	return f.inner.InstanceState(ctx, instanceID)
}

func (f fencedExports) Cleanup(ctx context.Context, shardID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.Cleanup(ctx, shardID)
}

type fencedStandbyLauncher struct{ inner StandbyLauncher }

func (f fencedStandbyLauncher) LaunchStandby(ctx context.Context, tier, az, claimFor string) (StandbyInstance, error) {
	if err := leaseGone(ctx); err != nil {
		return StandbyInstance{}, err
	}
	return f.inner.LaunchStandby(ctx, tier, az, claimFor)
}

func (f fencedStandbyLauncher) ListStandbyInstances(ctx context.Context) ([]StandbyInstance, error) {
	return f.inner.ListStandbyInstances(ctx)
}

func (f fencedStandbyLauncher) ClaimInstance(ctx context.Context, instanceID, shardID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.ClaimInstance(ctx, instanceID, shardID)
}

func (f fencedStandbyLauncher) ReleaseStandby(ctx context.Context, instanceID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.ReleaseStandby(ctx, instanceID)
}

func (f fencedStandbyLauncher) Terminate(ctx context.Context, instanceID string) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	return f.inner.Terminate(ctx, instanceID)
}

func (f fencedStandbyLauncher) CurrentImageID() string { return f.inner.CurrentImageID() }
