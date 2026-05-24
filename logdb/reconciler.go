package logdb

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/kevlarcon/weft/store"
)

// PrewarmLead is the interval before a current-window INGESTING
// shard's WindowEnd at which the reconciler starts pre-warming
// next-window shards. Pre-warm is always on — the replacement must
// be INGESTING before the current shard's window closes so the Op 3
// capacity gate can rotate it without a write-gap. Package-scoped
// rather than passed through the op signature: future
// Bucket.ShouldPrewarmNextWindow implementations might not use a
// lead duration at all (ingestion burn rate, disk-fill slope,
// etc.), so the policy shouldn't leak into every caller's
// signature. main.go can override at boot for per-env tuning; the
// default must comfortably exceed instance launch + configure time.
var PrewarmLead = 10 * time.Minute

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
	Bucket *Bucket
	// BucketServices is the (namespace_id, service) set the bucket
	// serves, read from log_groups by the snapshot builder. Op 1
	// (service-drift) diffs it against each INGESTING shard's membership
	// to find services onboarded after the shard christened. Pure
	// reconcile() can't do the DB read itself, so it's prefetched here.
	BucketServices       []store.ShardService
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

	// Op 1 — service-drift. Every INGESTING shard in the bucket must
	// serve every (namespace, service) the bucket serves (from
	// log_groups, prefetched into BucketServices). A service onboarded
	// after the shard christened is missing from its stamp; this adds
	// it (registry + shard_services) so writes and reads both reach it.
	// READ_ONLY/ARCHIVED shards stay frozen (they never accepted writes
	// for the new service, so widening their set would only widen query
	// fan-out).
	for _, e := range snap.Entries {
		if e.State != StateIngesting {
			continue
		}
		for _, s := range snap.BucketServices {
			if !e.serves(s.NamespaceID, s.Service) {
				ops = append(ops, AddServiceToShardOp{
					ShardID: e.ID, NamespaceID: s.NamespaceID, Service: s.Service,
				})
			}
		}
	}

	// Op 3 — wall-clock window-closed, capacity-gated. An INGESTING
	// shard past its WindowEnd (or whose span exceeds the bucket's
	// current WindowDuration — legacy shards under an older, wider
	// window config) should stop accepting writes and become
	// query-only.
	//
	// SAFETY INVARIANT: a shard never leaves INGESTING if doing so
	// drops the bucket below Target INGESTING shards. Rotation is
	// blocked until a replacement has launched AND reached INGESTING
	// (LAUNCHING does not count — it isn't accepting writes yet).
	// Without this gate a window roll opens a write-gap: every
	// current-window shard flips to READ_ONLY on time while the
	// replacement is still launching, leaving the bucket with no
	// shard accepting ingest. Oldest-WindowEnd shards rotate first so
	// the freshest INGESTING shard is the one kept hot.
	//
	// Runs before op 2 so last-minute app-drift reaches the shard
	// before it freezes.
	var eligible []*ShardEntry
	ingestingCount := 0
	for _, e := range snap.Entries {
		if e.State != StateIngesting {
			continue
		}
		ingestingCount++
		if e.WindowEnd.IsZero() {
			continue
		}
		pastWindowEnd := !snap.Now.Before(e.WindowEnd)
		staleWindow := snap.Bucket.WindowDuration > 0 &&
			e.WindowEnd.Sub(e.WindowStart) > snap.Bucket.WindowDuration
		if !pastWindowEnd && !staleWindow {
			continue
		}
		eligible = append(eligible, e)
	}
	// Rotate at most (ingesting − Target) shards so >= Target remain
	// INGESTING. Target 0 means "no ingest capacity required" → no
	// gate (all eligible rotate).
	budget := ingestingCount - snap.Bucket.Target
	if budget > len(eligible) {
		budget = len(eligible)
	}
	if budget > 0 {
		sort.SliceStable(eligible, func(i, j int) bool {
			return eligible[i].WindowEnd.Before(eligible[j].WindowEnd)
		})
		for _, e := range eligible[:budget] {
			ops = append(ops, RotateToReadOnlyOp{ShardID: e.ID})
		}
	}

	// Op 2 — under-capacity. Emit one LaunchShardOp per shard
	// missing from the target. Each op launches exactly one shard;
	// apply is idempotent at the list level because the next
	// tick's snapshot sees the new LAUNCHING/INGESTING row and the
	// gap shrinks.
	//
	// All INGESTING shards count toward target — a past-window
	// shard still accepts writes (Op 3's gate keeps it INGESTING
	// until its replacement is up) and pre-warm, which is always
	// on, provisions that replacement ahead of WindowEnd. Op 2
	// only back-stops genuine under-capacity (a shard that died or
	// never launched). ingestingCount comes from the Op 3 walk
	// above so the two ops stay symmetric. A row in the DB but
	// absent from the registry no longer counts toward target-met;
	// the reconciler launches a replacement, and the stuck row gets
	// cleaned up when its instance is rebound or the reaper
	// archives it.
	effective := snap.LaunchingCount + ingestingCount
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
					Bucket:      snap.Bucket,
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
	Bucket *Bucket
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
	Bucket      *Bucket
	WindowStart time.Time
	WindowEnd   time.Time
}

func (o PrewarmNextWindowOp) Describe() string {
	id := int64(0)
	if o.Bucket != nil {
		id = o.Bucket.ID
	}
	return fmt.Sprintf("pre-warm bucket %d window starting %s", id, o.WindowStart.Format(time.RFC3339))
}

func (o PrewarmNextWindowOp) Apply(_ context.Context, p *Provisioner) error {
	if o.Bucket == nil {
		return fmt.Errorf("%s: nil bucket", o.Describe())
	}
	if _, err := p.mintLaunchingRowForWindow(o.Bucket, o.WindowStart, o.WindowEnd); err != nil {
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

// AddServiceToShardOp records one (namespace, service) onto one
// INGESTING shard (registry + shard_services). Idempotent and
// state-gated: a shard that has since flipped to READ_ONLY between
// snapshot and apply is left alone.
type AddServiceToShardOp struct {
	ShardID     string
	NamespaceID uint
	Service     string
}

func (o AddServiceToShardOp) Describe() string {
	return fmt.Sprintf("add service %d/%q to shard %s", o.NamespaceID, o.Service, o.ShardID)
}

func (o AddServiceToShardOp) Apply(_ context.Context, p *Provisioner) error {
	found, _ := p.registry.AddServiceToShard(o.ShardID, o.NamespaceID, o.Service)
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
func (p *Provisioner) snapshotForBucket(bucket *Bucket) ReconcilerSnapshot {
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

	// Prefetch the bucket's services from log_groups for op 1's
	// service-drift diff (reconcile() is pure and can't read the DB).
	if p.config.ShardOps != nil {
		if svcs, err := p.config.ShardOps.ServicesForBucket(bucket.ID); err != nil {
			log.Printf("reconciler: bucket %d services: %v", bucket.ID, err)
		} else {
			snap.BucketServices = svcs
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
// periodic catchup and from OnboardService for immediate propagation.
// Concurrent callers are serialised by per-op idempotency and
// state re-checks, not a lock here.
//
// Logs a one-line summary at the start of every pass so the
// reconciler's view of the bucket (target / ingesting / launching /
// future-launching, plus emitted op count) is greppable in prod.
// During a refresh-induced ABANDON storm this is the line that
// answers "did the reconciler think anything was wrong yet?"
func (p *Provisioner) ReconcileBucket(ctx context.Context, bucket *Bucket) {
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
