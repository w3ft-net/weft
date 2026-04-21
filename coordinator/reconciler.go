package coordinator

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ReconcileOp is one idempotent action the reconciler wants to take
// to close a drift between desired state (bucket config) and actual
// state (registry + shard_records). `reconcile()` computes a list of
// ops from a point-in-time snapshot; each Apply does the side
// effect. Ops re-check state on Apply so two ticks that both compute
// "do X" can safely both run — the loser of the race becomes a
// no-op.
type ReconcileOp interface {
	Describe() string
	Apply(ctx context.Context, p *Provisioner) error
}

// ReconcilerSnapshot is the state a single reconciler pass reads for
// one bucket. Feed the snapshot into reconcile() → ops list, with no
// side effects.
//
// LaunchingCount and IngestingCount are pre-computed by
// snapshotForBucket so reconcile() is pure arithmetic. The async
// vs. sync-launcher asymmetry lives in the snapshot builder: async
// launchers count from `shard_records` (authoritative when an
// instance hasn't heartbeated yet), sync launchers count from the
// registry (there's no LAUNCHING intermediate state to miss).
type ReconcilerSnapshot struct {
	Bucket         *AppBucket
	Entries        []*ShardEntry // registry entries with BucketID == Bucket.ID
	LaunchingCount int           // shard_records rows in this bucket with state=LAUNCHING
	IngestingCount int           // authoritative INGESTING count (DB for async, registry for sync)
	Now            time.Time
}

// reconcile computes the op list for one bucket. Pure.
func reconcile(snap ReconcilerSnapshot) []ReconcileOp {
	if snap.Bucket == nil {
		return nil
	}
	var ops []ReconcileOp

	// Op 1 — app-drift. Every INGESTING shard in the bucket must
	// own every app in bucket.Apps. READ_ONLY/ARCHIVED shards stay
	// frozen (they never accepted writes for the new app, so
	// widening their app set would only widen query fan-out).
	for _, e := range snap.Entries {
		if e.State != StateIngesting {
			continue
		}
		have := make(map[string]bool, len(e.Apps))
		for _, a := range e.Apps {
			have[a] = true
		}
		for _, a := range snap.Bucket.Apps {
			if !have[a] {
				ops = append(ops, AddAppToShardOp{ShardID: e.ID, App: a})
			}
		}
	}

	// Op 3 — wall-clock window-closed. An INGESTING shard past its
	// WindowEnd stops accepting writes and becomes query-only.
	// Runs before op 2 so last-minute app-drift reaches the shard
	// before it freezes; replacement launches happen naturally on
	// the next tick once IngestingCount drops.
	for _, e := range snap.Entries {
		if e.State != StateIngesting {
			continue
		}
		if e.WindowEnd.IsZero() {
			continue
		}
		if snap.Now.Before(e.WindowEnd) {
			continue
		}
		ops = append(ops, RotateToReadOnlyOp{ShardID: e.ID})
	}

	// Op 2 — under-capacity. Emit one LaunchShardOp per shard
	// missing from the target. Each op launches exactly one shard;
	// apply is idempotent at the list level because the next
	// tick's snapshot sees the new LAUNCHING/INGESTING row and the
	// gap shrinks.
	effective := snap.LaunchingCount + snap.IngestingCount
	for i := effective; i < snap.Bucket.Target; i++ {
		ops = append(ops, LaunchShardOp{Bucket: snap.Bucket})
	}

	// Op 5 — retention-archive. A READ_ONLY shard whose WindowEnd
	// is older than bucket.RetentionPeriod gets archived. Zero
	// retention means "archive disabled for this bucket."
	if snap.Bucket.RetentionPeriod > 0 {
		for _, e := range snap.Entries {
			if e.State != StateReadOnly {
				continue
			}
			if e.WindowEnd.IsZero() {
				continue
			}
			if snap.Now.Sub(e.WindowEnd) <= snap.Bucket.RetentionPeriod {
				continue
			}
			ops = append(ops, ArchiveShardOp{ShardID: e.ID})
		}
	}

	// Op 6 — terminate-on-archive. An ARCHIVED shard whose EC2
	// instance hasn't been reclaimed yet. TerminateInstanceInAutoScalingGroup
	// decrements desired_capacity atomically so the coordinator's
	// view and the ASG's stay consistent. The archive op in the
	// same tick doesn't emit this op for the same shard (snapshot
	// predates the archive); terminate fires on the following tick.
	for _, e := range snap.Entries {
		if e.State != StateArchived {
			continue
		}
		if e.InstanceID == "" {
			continue
		}
		ops = append(ops, TerminateArchivedInstanceOp{ShardID: e.ID})
	}

	return ops
}

// LaunchShardOp provisions one additional shard for a bucket. The
// async-vs-sync split lives in provisionForBucket: async launchers
// INSERT a LAUNCHING row and call the ASG; sync launchers launch +
// Configure inline and register the INGESTING entry directly. Apply
// doesn't re-check capacity — if two concurrent ticks both emit
// this op the second launch just overshoots, and the tick after
// converges by emitting zero.
type LaunchShardOp struct {
	Bucket *AppBucket
}

func (o LaunchShardOp) Describe() string {
	if o.Bucket == nil {
		return "launch shard for <nil bucket>"
	}
	return fmt.Sprintf("launch 1 shard for bucket %s(%d)", o.Bucket.Label, o.Bucket.ID)
}

func (o LaunchShardOp) Apply(ctx context.Context, p *Provisioner) error {
	if o.Bucket == nil {
		return fmt.Errorf("LaunchShardOp: nil bucket")
	}
	p.provisionForBucket(ctx, o.Bucket)
	return nil
}

// TerminateArchivedInstanceOp terminates the EC2 instance still
// running behind an ARCHIVED shard. The shard's data has already
// been exported to S3 and its EBS volume retired; all that's left
// is to reclaim the compute. TerminateInstanceInAutoScalingGroup
// (not ec2:TerminateInstances) is used so the ASG's
// desired_capacity decrements atomically and the coordinator's
// view stays consistent.
//
// Idempotent via Apply's state + InstanceID re-checks. After a
// successful terminate the op nulls entry.InstanceID so the next
// tick's snapshot doesn't re-emit.
type TerminateArchivedInstanceOp struct {
	ShardID string
}

func (o TerminateArchivedInstanceOp) Describe() string {
	return fmt.Sprintf("terminate archived instance for shard %s", o.ShardID)
}

func (o TerminateArchivedInstanceOp) Apply(ctx context.Context, p *Provisioner) error {
	entry := p.registry.Get(o.ShardID)
	if entry == nil {
		return fmt.Errorf("%s: entry not in registry", o.Describe())
	}
	if entry.State != StateArchived {
		return nil // state changed since snapshot — not our concern anymore
	}
	if entry.InstanceID == "" {
		return nil // already terminated or never on an instance we track
	}
	if p.launcher != nil {
		if err := p.launcher.TerminateInstance(ctx, entry.InstanceID, entry.Addr); err != nil {
			return fmt.Errorf("%s: terminate %s: %w", o.Describe(), entry.InstanceID, err)
		}
	}
	entry.InstanceID = ""
	entry.Addr = ""
	p.registry.PersistState(entry)
	return nil
}

// ArchiveShardOp archives one READ_ONLY shard past its bucket's
// retention_period. Apply delegates to provisioner.archive(), which
// handles Parquet export, ColdShard swap, and volume retirement.
// The ParquetDir config gate lives in Apply (coordinator-level
// concern, not a per-bucket decision); a shard no longer in
// READ_ONLY short-circuits.
type ArchiveShardOp struct {
	ShardID string
}

func (o ArchiveShardOp) Describe() string {
	return fmt.Sprintf("archive shard %s", o.ShardID)
}

func (o ArchiveShardOp) Apply(_ context.Context, p *Provisioner) error {
	if p.config.ParquetDir == "" {
		return nil // coordinator not wired for export; skip
	}
	entry := p.registry.Get(o.ShardID)
	if entry == nil {
		return fmt.Errorf("%s: entry not in registry", o.Describe())
	}
	if entry.State != StateReadOnly {
		return nil
	}
	p.archive(entry)
	return nil
}

// RotateToReadOnlyOp flips one INGESTING shard to READ_ONLY when
// wall-clock time has passed its WindowEnd. Idempotent: a shard
// already READ_ONLY (or beyond) short-circuits.
type RotateToReadOnlyOp struct {
	ShardID string
}

func (o RotateToReadOnlyOp) Describe() string {
	return fmt.Sprintf("rotate shard %s → READ_ONLY", o.ShardID)
}

func (o RotateToReadOnlyOp) Apply(_ context.Context, p *Provisioner) error {
	entry := p.registry.Get(o.ShardID)
	if entry == nil {
		return fmt.Errorf("%s: entry not in registry", o.Describe())
	}
	if entry.State != StateIngesting {
		return nil
	}
	entry.State = StateReadOnly
	p.registry.PersistState(entry)
	return nil
}

// AddAppToShardOp appends one app to one INGESTING shard. Idempotent
// and state-gated: a shard that has since flipped to READ_ONLY
// between snapshot and apply is left alone.
type AddAppToShardOp struct {
	ShardID string
	App     string
}

func (o AddAppToShardOp) Describe() string {
	return fmt.Sprintf("add app %q to shard %s", o.App, o.ShardID)
}

func (o AddAppToShardOp) Apply(_ context.Context, p *Provisioner) error {
	entry := p.registry.Get(o.ShardID)
	if entry == nil {
		return fmt.Errorf("%s: entry not in registry", o.Describe())
	}
	if entry.State != StateIngesting {
		return nil
	}
	for _, a := range entry.Apps {
		if a == o.App {
			return nil
		}
	}
	entry.Apps = append(entry.Apps, o.App)
	p.registry.PersistState(entry)
	return nil
}

// snapshotForBucket assembles a ReconcilerSnapshot from the live
// registry and store. Async launchers count LAUNCHING + INGESTING
// from shard_records (authoritative while a heartbeat is still in
// flight); sync launchers count INGESTING from the registry
// directly because they skip the LAUNCHING state entirely.
func (p *Provisioner) snapshotForBucket(bucket *AppBucket) ReconcilerSnapshot {
	snap := ReconcilerSnapshot{
		Bucket: bucket,
		Now:    time.Now().UTC(),
	}
	for _, e := range p.registry.Entries() {
		if e.BucketID == bucket.ID {
			snap.Entries = append(snap.Entries, e)
		}
	}

	if isLaunchingLauncher(p.launcher) {
		if p.config.ShardOps != nil {
			if n, err := p.config.ShardOps.CountShardsInBucket(bucket.ID, []string{string(StateLaunching)}); err != nil {
				log.Printf("reconciler: bucket %d LAUNCHING count: %v", bucket.ID, err)
			} else {
				snap.LaunchingCount = n
			}
			if n, err := p.config.ShardOps.CountShardsInBucket(bucket.ID, []string{string(StateIngesting)}); err != nil {
				log.Printf("reconciler: bucket %d INGESTING count: %v", bucket.ID, err)
			} else {
				snap.IngestingCount = n
			}
		}
	} else {
		for _, e := range snap.Entries {
			if e.State == StateIngesting {
				snap.IngestingCount++
			}
		}
	}

	return snap
}

// ReconcileBucket runs one pass of the reconciler for a single
// bucket, applying each op in order. Called from the ticker for
// periodic catchup and from OnboardApp for immediate propagation.
// Concurrent callers are serialised by per-op idempotency and
// state re-checks, not a lock here.
func (p *Provisioner) ReconcileBucket(ctx context.Context, bucket *AppBucket) {
	snap := p.snapshotForBucket(bucket)
	for _, op := range reconcile(snap) {
		if err := op.Apply(ctx, p); err != nil {
			log.Printf("reconciler: apply %s: %v", op.Describe(), err)
		}
	}
}
