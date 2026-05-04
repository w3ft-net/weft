package coordinator

import (
	"context"
	"fmt"
	"log"
	"time"
)

// PrewarmLead is the interval before a current-window INGESTING
// shard's WindowEnd at which the reconciler starts pre-warming
// next-window shards. Zero disables pre-warm entirely. Package-
// scoped rather than passed through the op signature: future
// AppBucket.ShouldPrewarmNextWindow implementations might not use a
// lead duration at all (ingestion burn rate, disk-fill slope,
// etc.), so the policy shouldn't leak into every caller's
// signature. main.go can override at boot for per-env tuning.
var PrewarmLead time.Duration

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
	Bucket               *AppBucket
	Entries              []*ShardEntry // registry entries with BucketID == Bucket.ID
	LaunchingCount       int           // shard_records rows in this bucket with state=LAUNCHING
	IngestingCount       int           // authoritative INGESTING count (DB for async, registry for sync)
	FutureLaunchingCount int           // LAUNCHING rows with window_start > current (pre-warm)
	IsAsyncLauncher      bool          // whether the launcher uses the LAUNCHING-row handoff
	Now                  time.Time
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
	//
	// IngestingCount is derived from snap.Entries here rather than
	// trusting any pre-computed snap.IngestingCount field — Op 3
	// (rotate at WindowEnd) walks Entries, so deriving Op 2's count
	// from the same source keeps the two ops symmetric. A row in
	// the DB but absent from the registry no longer counts toward
	// target-met; the reconciler launches a replacement, and the
	// stuck row gets cleaned up when its instance is rebound or the
	// reaper archives it.
	ingesting := 0
	for _, e := range snap.Entries {
		if e.State == StateIngesting {
			ingesting++
		}
	}
	effective := snap.LaunchingCount + ingesting
	for i := effective; i < snap.Bucket.Target; i++ {
		ops = append(ops, LaunchShardOp{Bucket: snap.Bucket})
	}

	// Op 4 — pre-warm next window. When a current-window INGESTING
	// shard is within the bucket's pre-warm threshold, mint
	// bucket.Target LAUNCHING rows for the next window so the ASG
	// can bring up replacement instances before the rotation flips
	// the current-window shards to READ_ONLY. Async launchers only —
	// sync launchers configure inline with no LAUNCHING intermediate.
	if snap.IsAsyncLauncher && snap.FutureLaunchingCount == 0 {
		if snap.Bucket.ShouldPrewarmNextWindow(snap.Now, snap.Entries) {
			nextStart, nextEnd := snap.Bucket.NextWindow(snap.Now)
			for i := 0; i < snap.Bucket.Target; i++ {
				ops = append(ops, PrewarmNextWindowOp{
					BucketID:    snap.Bucket.ID,
					WindowStart: nextStart,
					WindowEnd:   nextEnd,
				})
			}
		}
	}

	// Op 5 — retention-archive. A READ_ONLY shard whose WindowEnd
	// is older than bucket.WindowDuration gets archived. Zero
	// duration means "archive disabled for this bucket."
	if snap.Bucket.WindowDuration > 0 {
		for _, e := range snap.Entries {
			if e.State != StateReadOnly {
				continue
			}
			if e.WindowEnd.IsZero() {
				continue
			}
			if snap.Now.Sub(e.WindowEnd) <= snap.Bucket.WindowDuration {
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

// PrewarmNextWindowOp inserts a LAUNCHING row for a bucket's next
// window while the current window is still INGESTING. This closes
// the write-gap at window rotation: new-window instances are
// already up and serving by the time op 3 flips the current-window
// shards to READ_ONLY. Apply calls mintLaunchingRowForWindow,
// persisting an intent row the lifecycle hook will claim when the
// ASG brings up the instance.
type PrewarmNextWindowOp struct {
	BucketID    int64
	WindowStart time.Time
	WindowEnd   time.Time
}

func (o PrewarmNextWindowOp) Describe() string {
	return fmt.Sprintf("pre-warm bucket %d window starting %s", o.BucketID, o.WindowStart.Format(time.RFC3339))
}

func (o PrewarmNextWindowOp) Apply(_ context.Context, p *Provisioner) error {
	if p.Buckets == nil {
		return fmt.Errorf("%s: no BucketManager", o.Describe())
	}
	bucket, err := p.Buckets.ByID(o.BucketID)
	if err != nil {
		return fmt.Errorf("%s: lookup bucket %d: %w", o.Describe(), o.BucketID, err)
	}
	if bucket == nil {
		return fmt.Errorf("%s: bucket %d not found", o.Describe(), o.BucketID)
	}
	if _, err := p.mintLaunchingRowForWindow(bucket, o.WindowStart, o.WindowEnd); err != nil {
		return fmt.Errorf("%s: mint LAUNCHING: %w", o.Describe(), err)
	}
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
// window_duration. Apply delegates to provisioner.archive(), which
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
	found, _ := p.registry.AddAppToShard(o.ShardID, o.App)
	if !found {
		return fmt.Errorf("%s: entry not in registry", o.Describe())
	}
	return nil
}

// snapshotForBucket assembles a ReconcilerSnapshot from the live
// registry and store. Async launchers count LAUNCHING + INGESTING
// from shard_records (authoritative while a heartbeat is still in
// flight); sync launchers count INGESTING from the registry
// directly because they skip the LAUNCHING state entirely.
func (p *Provisioner) snapshotForBucket(bucket *AppBucket) ReconcilerSnapshot {
	snap := ReconcilerSnapshot{
		Bucket:          bucket,
		Now:             time.Now().UTC(),
		IsAsyncLauncher: isLaunchingLauncher(p.launcher),
	}
	for _, e := range p.registry.Entries() {
		if e.BucketID == bucket.ID {
			snap.Entries = append(snap.Entries, e)
		}
	}

	// IngestingCount and LAUNCHING-related counts are answered from
	// different sources by design:
	//
	//   - INGESTING: walked from snap.Entries (registry). Op 3
	//     (rotate-at-WindowEnd) also walks Entries, so the two ops
	//     stay symmetric — counting INGESTING from the DB while
	//     rotating from the registry was the asymmetry that produced
	//     the prod orphan-christen loop on 2026-04-29 (a stale row
	//     in the DB but absent from the registry counted toward the
	//     target while never getting rotated). The registry is now
	//     fully populated from the DB at boot via LoadFromStore, so
	//     the registry walk is authoritative.
	//
	//   - LAUNCHING (current and future-window): counted from the DB
	//     because LAUNCHING rows live in shard_records with no
	//     registry presence — they're the lifecycle hook's intent
	//     rows, claimed by instance_id at hook time, only transitioning
	//     to INGESTING (and entering the registry) once the shard
	//     christens.
	for _, e := range snap.Entries {
		if e.State == StateIngesting {
			snap.IngestingCount++
		}
	}
	if snap.IsAsyncLauncher && p.config.ShardOps != nil {
		if n, err := p.config.ShardOps.CountShardsInBucket(bucket.ID, []string{string(StateLaunching)}); err != nil {
			log.Printf("reconciler: bucket %d LAUNCHING count: %v", bucket.ID, err)
		} else {
			snap.LaunchingCount = n
		}
		currentStart, _ := bucket.CurrentWindow(snap.Now)
		if n, err := p.config.ShardOps.CountShardsInBucketForFutureWindows(bucket.ID, currentStart); err != nil {
			log.Printf("reconciler: bucket %d future-LAUNCHING count: %v", bucket.ID, err)
		} else {
			snap.FutureLaunchingCount = n
		}
	}

	return snap
}

// ReconcileBucket runs one pass of the reconciler for a single
// bucket, applying each op in order. Called from the ticker for
// periodic catchup and from OnboardApp for immediate propagation.
// Concurrent callers are serialised by per-op idempotency and
// state re-checks, not a lock here.
//
// Logs a one-line summary at the start of every pass so the
// reconciler's view of the bucket (target / ingesting / launching /
// future-launching, plus emitted op count) is greppable in prod.
// During a refresh-induced ABANDON storm this is the line that
// answers "did the reconciler think anything was wrong yet?"
func (p *Provisioner) ReconcileBucket(ctx context.Context, bucket *AppBucket) {
	snap := p.snapshotForBucket(bucket)
	ops := reconcile(snap)
	log.Printf("reconciler: bucket=%s id=%d tier=%s target=%d ingesting=%d launching=%d future_launching=%d ops=%d",
		bucket.Label, bucket.ID, bucket.Tier,
		bucket.Target,
		ingestingFromEntries(snap.Entries),
		snap.LaunchingCount,
		snap.FutureLaunchingCount,
		len(ops),
	)
	for _, op := range ops {
		if err := op.Apply(ctx, p); err != nil {
			log.Printf("reconciler: apply %s: %v", op.Describe(), err)
		}
	}
}

// ingestingFromEntries returns the count of INGESTING entries in a
// snapshot's registry slice. Mirrors reconcile()'s internal walk so
// the per-tick log line and the under-capacity arithmetic agree on
// what counts.
func ingestingFromEntries(entries []*ShardEntry) int {
	n := 0
	for _, e := range entries {
		if e.State == StateIngesting {
			n++
		}
	}
	return n
}
