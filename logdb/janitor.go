package logdb

import (
	"context"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"regexp"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// RetentionGCDelay is how long a superseded/retired parquet object lingers
// as a tombstone before the GC sweep deletes it — a grace window so an
// in-flight cold read (which resolved the old path at query start) isn't
// yanked mid-stream.
const RetentionGCDelay = time.Hour

// RetentionGrace is the cushion added to every tier's advertised
// retention before the janitor will delete a row. It absorbs clock skew
// and softens the downgrade-then-re-upgrade case (a customer who drops
// to a shorter tier and back doesn't instantly lose the in-between data).
// Deletion is irreversible, so we err generous. See
// docs/plans/retention-janitor.md.
const RetentionGrace = 7 * 24 * time.Hour

// RetentionResolver maps a customer — the id in logs.customer_id and
// shard_archive_stats.org_id — to its log-retention period. Built once
// per janitor pass from store.ActiveCustomerTiers so the per-shard
// classification below is pure in-memory map lookups, not a query per
// customer.
//
// A customer absent from the map (no active subscription: Stripe lag,
// provisioning race, cancelled) resolves ok=false, and the classifier
// treats that as "never delete". This is the deliberate divergence from
// the query-side clamp, which can safely default an unknown customer to
// trial (hiding data is reversible); deleting it is not.
type RetentionResolver struct {
	tiers map[store.OrgID]store.Tier
}

// NewRetentionResolver wraps an already-loaded customer→tier map.
func NewRetentionResolver(tiers map[store.OrgID]store.Tier) *RetentionResolver {
	return &RetentionResolver{tiers: tiers}
}

// Retention returns a customer's advertised log-retention period and
// ok=true when the customer has a known active tier. ok=false means
// "unmapped — preserve this customer's data". Grace is applied by the
// caller (the classifier), not baked in here.
func (r *RetentionResolver) Retention(customer store.OrgID) (time.Duration, bool) {
	tier, ok := r.tiers[customer]
	if !ok {
		return 0, false
	}
	return store.LogRetention(tier), true
}

// Cutoffs derives the per-customer deletion boundaries to hand the
// rewrite engine for one shard's stats: every mapped customer present
// gets cutoff = now − retention(tier) − grace. Unmapped customers are
// omitted, which is exactly how the rewrite preserves them (a customer
// with no cutoff keeps all its rows). The result is empty when no present
// customer is mapped (nothing to delete).
func (r *RetentionResolver) Cutoffs(stats []ShardArchiveStat, now time.Time, grace time.Duration) []shard.CustomerCutoff {
	var out []shard.CustomerCutoff
	for _, st := range stats {
		retention, ok := r.Retention(st.OrgID)
		if !ok {
			continue // unmapped: preserved, no cutoff
		}
		out = append(out, shard.CustomerCutoff{
			CustomerID: int64(st.OrgID),
			Cutoff:     now.Add(-retention - grace),
		})
	}
	return out
}

// AllCutoffs derives a cutoff for every mapped customer — used for an
// Unknown shard whose archive stats weren't captured, so we don't know
// which customers it holds. The rewrite only drops rows for customers
// actually present (the LEFT JOIN misses the rest), so an over-broad
// cutoff set is harmless.
func (r *RetentionResolver) AllCutoffs(now time.Time, grace time.Duration) []shard.CustomerCutoff {
	out := make([]shard.CustomerCutoff, 0, len(r.tiers))
	for cust, tier := range r.tiers {
		out = append(out, shard.CustomerCutoff{
			CustomerID: int64(cust),
			Cutoff:     now.Add(-store.LogRetention(tier) - grace),
		})
	}
	return out
}

// ShardDisposition is the planner's verdict for one archived shard's
// parquet, decided from shard_archive_stats alone — no S3 read.
type ShardDisposition int

const (
	// DispositionKeep: nothing in the file is over-retained; leave it.
	DispositionKeep ShardDisposition = iota
	// DispositionDelete: every row is over-retained and every customer
	// present is mapped — drop the whole object, no rewrite needed.
	DispositionDelete
	// DispositionRewrite: the file mixes expired and live (or preserved)
	// rows — rewrite it, dropping only the expired ones.
	DispositionRewrite
	// DispositionUnknown: no stats were captured for this shard (archived
	// before stats existed, or by an un-rolled old shard binary). The
	// rewriter must scan the parquet to decide. Never deleted blind.
	DispositionUnknown
)

func (d ShardDisposition) String() string {
	switch d {
	case DispositionKeep:
		return "keep"
	case DispositionDelete:
		return "delete"
	case DispositionRewrite:
		return "rewrite"
	default:
		return "unknown"
	}
}

// ClassifyArchivedShard decides what the janitor should do with an
// archived shard's parquet from its per-customer stats. A row is eligible
// for deletion when ts < now − retention(tier) − grace; the cutoff is
// applied per customer. An unmapped customer (resolver ok=false) is
// always preserved, so its mere presence downgrades a would-be Delete to
// a Rewrite.
//
// No stats → Unknown (the file must be scanned, never deleted blind).
func ClassifyArchivedShard(stats []ShardArchiveStat, r *RetentionResolver, now time.Time, grace time.Duration) ShardDisposition {
	if len(stats) == 0 {
		return DispositionUnknown
	}
	var hasExpired, hasLive bool
	for _, st := range stats {
		retention, ok := r.Retention(st.OrgID)
		if !ok {
			hasLive = true // unmapped: preserve all of this customer's rows
			continue
		}
		cutoff := now.Add(-retention - grace)
		switch {
		case st.MaxTs.Before(cutoff):
			hasExpired = true // entire span past the cutoff
		case !st.MinTs.Before(cutoff):
			hasLive = true // entire span within retention
		default:
			hasExpired, hasLive = true, true // straddles the cutoff
		}
	}
	switch {
	case hasExpired && !hasLive:
		return DispositionDelete
	case hasExpired && hasLive:
		return DispositionRewrite
	default:
		return DispositionKeep
	}
}

// PlannedShard pairs an archived shard with the work the janitor owes it.
type PlannedShard struct {
	ShardID     string
	ParquetPath string
	Disposition ShardDisposition
}

// PlanRetention classifies every ARCHIVED shard against the current
// subscription state and returns the ones that need work (Keep shards are
// omitted). Postgres-only: it reads shard_records, shard_archive_stats,
// and the customer→tier map — the actual S3 delete/rewrite is a later
// step. now and grace are injected for testability; production passes
// the wall clock and RetentionGrace.
func (s ShardStore) PlanRetention(now time.Time, grace time.Duration) ([]PlannedShard, error) {
	tiers, err := s.ActiveCustomerTiers()
	if err != nil {
		return nil, err
	}
	resolver := NewRetentionResolver(tiers)

	shards, err := s.listArchivedShards()
	if err != nil {
		return nil, err
	}
	statsByShard, err := s.archiveStatsByShard()
	if err != nil {
		return nil, err
	}

	var plan []PlannedShard
	for _, sh := range shards {
		disp := ClassifyArchivedShard(statsByShard[sh.ID], resolver, now, grace)
		if disp == DispositionKeep {
			continue
		}
		plan = append(plan, PlannedShard{
			ShardID:     sh.ID,
			ParquetPath: sh.ParquetPath,
			Disposition: disp,
		})
	}
	return plan, nil
}

// listArchivedShards returns only the terminal ARCHIVED shards — the ones
// whose data lives solely in S3 parquet and is the janitor's to manage.
func (s ShardStore) listArchivedShards() ([]*Shard, error) {
	var shards []*Shard
	err := s.db().Where("state = ?", string(StateArchived)).Find(&shards).Error
	return shards, err
}

// archiveStatsByShard loads all shard_archive_stats rows once and groups
// them by shard, so PlanRetention does one query instead of one per
// shard.
func (s ShardStore) archiveStatsByShard() (map[string][]ShardArchiveStat, error) {
	var rows []ShardArchiveStat
	if err := s.db().Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string][]ShardArchiveStat)
	for _, r := range rows {
		out[r.ShardID] = append(out[r.ShardID], r)
	}
	return out, nil
}

// ParquetTombstone is a superseded or retired parquet object awaiting GC.
// A separate table (not columns on shard_records) so it outlives the
// shard row when a fully-expired shard is retired to GONE.
type ParquetTombstone struct {
	ID          int64     `gorm:"primaryKey"`
	Path        string    `gorm:"column:path"`
	ShardID     string    `gorm:"column:shard_id"`
	Reason      string    `gorm:"column:reason"`
	DeleteAfter time.Time `gorm:"column:delete_after"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (*ParquetTombstone) TableName() string { return "parquet_tombstones" }

// PurgeDeps carries the finalizer's collaborators. Injected so ExecutePurge
// is testable without S3 or the live registry.
type PurgeDeps struct {
	// Compactor runs the heavy parquet work (rewrite, capture). In prod a
	// shard.LambdaCompactor offloads it so the coordinator stays free of
	// DuckDB + S3 I/O; tests use shard.LocalCompactor over local parquet.
	Compactor shard.Compactor
	// Grace is the retention cushion (RetentionGrace in production).
	Grace time.Duration
	// GCDelay is how long a tombstoned object lingers before GC
	// (RetentionGCDelay in production).
	GCDelay time.Duration
	// Now is the clock (injected for tests).
	Now func() time.Time
	// Retire transitions a fully-expired shard out of service. In the
	// coordinator this fires the shard SM's purge trigger (→ GONE, which
	// removes it from the registry and deletes the row); the FSM stays the
	// owner of the transition. The parquet is already tombstoned in
	// parquet_tombstones, which survives the row's deletion.
	Retire func(shardID string) error
}

// PurgeOutcome is what ExecutePurge did, so the caller knows whether the
// in-memory registry entry needs a follow-up (a rewritten shard's
// ParquetPath changed; a retired shard was removed from the registry by
// the GONE transition).
type PurgeOutcome int

const (
	OutcomeNoop            PurgeOutcome = iota
	OutcomeStatsBackfilled              // Unknown → stats captured, nothing moved
	OutcomeRetired                      // shard fully expired → tombstoned + GONE
	OutcomeRewritten                    // parquet_path swapped → registry needs the new path
)

// PurgeResult reports ExecutePurge's outcome. NewPath is set only for
// OutcomeRewritten.
type PurgeResult struct {
	Outcome PurgeOutcome
	NewPath string
}

// ExecutePurge carries out one planned disposition against S3 + Postgres.
// Keep shards never reach here (PlanRetention omits them).
//
//   - Unknown: backfill shard_archive_stats from the file; a later pass
//     reclassifies it for real (no rewrite this pass).
//   - Delete: tombstone the whole object and retire the shard.
//   - Rewrite: rewrite to a new key, swap parquet_path, refresh stats, and
//     tombstone the old key — unless nothing survives, which collapses to
//     the Delete path.
func (s ShardStore) ExecutePurge(p PlannedShard, resolver *RetentionResolver, deps PurgeDeps) (PurgeResult, error) {
	now := deps.Now()
	switch p.Disposition {
	case DispositionUnknown:
		stats, err := deps.Compactor.CaptureStats(p.ParquetPath)
		if err != nil {
			return PurgeResult{}, fmt.Errorf("capture stats %s: %w", p.ShardID, err)
		}
		if err := s.RecordArchiveStats(p.ShardID, stats); err != nil {
			return PurgeResult{}, err
		}
		if err := s.markPurged(p.ShardID, now); err != nil {
			return PurgeResult{}, err
		}
		return PurgeResult{Outcome: OutcomeStatsBackfilled}, nil

	case DispositionDelete:
		if err := s.retire(p, deps, now); err != nil {
			return PurgeResult{}, err
		}
		return PurgeResult{Outcome: OutcomeRetired}, nil

	case DispositionRewrite:
		stats, err := s.shardArchiveStats(p.ShardID)
		if err != nil {
			return PurgeResult{}, err
		}
		dst := rewriteDestPath(p.ParquetPath, now)
		res, err := deps.Compactor.Rewrite(p.ParquetPath, dst, resolver.Cutoffs(stats, now, deps.Grace))
		if err != nil {
			return PurgeResult{}, fmt.Errorf("rewrite %s: %w", p.ShardID, err)
		}
		if res.RowsKept == 0 {
			if err := s.retire(p, deps, now); err != nil {
				return PurgeResult{}, err
			}
			return PurgeResult{Outcome: OutcomeRetired}, nil
		}
		if err := s.swap(p, dst, res.Stats, deps, now); err != nil {
			return PurgeResult{}, err
		}
		return PurgeResult{Outcome: OutcomeRewritten, NewPath: dst}, nil

	default:
		return PurgeResult{}, nil
	}
}

// retire tombstones a fully-expired shard's whole object and fires the
// SM retirement. Tombstone first so it survives the row deletion Retire
// triggers; if Retire fails the shard stays ARCHIVED and is replanned
// next pass (a duplicate tombstone is harmless — GC of a gone object is a
// no-op).
func (s ShardStore) retire(p PlannedShard, deps PurgeDeps, now time.Time) error {
	if err := s.recordTombstone(p.ParquetPath, p.ShardID, "retention-delete", now.Add(deps.GCDelay)); err != nil {
		return err
	}
	return deps.Retire(p.ShardID)
}

// swap cuts a rewritten shard over to its new parquet: point parquet_path
// at the survivors, refresh the stats, then tombstone the old object. The
// pointer swap precedes the tombstone so a crash between them only leaks
// the old object (GC never deletes a still-referenced path).
func (s ShardStore) swap(p PlannedShard, dst string, stats []shard.CustomerStat, deps PurgeDeps, now time.Time) error {
	if err := s.db().Model(&Shard{}).Where("id = ?", p.ShardID).
		Select("ParquetPath", "LastPurgedAt").
		Updates(&Shard{ParquetPath: dst, LastPurgedAt: &now}).Error; err != nil {
		return fmt.Errorf("swap parquet_path %s: %w", p.ShardID, err)
	}
	if err := s.RecordArchiveStats(p.ShardID, stats); err != nil {
		return err
	}
	return s.recordTombstone(p.ParquetPath, p.ShardID, "rewrite", now.Add(deps.GCDelay))
}

func (s ShardStore) markPurged(shardID string, now time.Time) error {
	return s.db().Model(&Shard{}).Where("id = ?", shardID).
		Select("LastPurgedAt").Updates(&Shard{LastPurgedAt: &now}).Error
}

func (s ShardStore) recordTombstone(path, shardID, reason string, deleteAfter time.Time) error {
	return s.db().Create(&ParquetTombstone{
		Path: path, ShardID: shardID, Reason: reason, DeleteAfter: deleteAfter,
	}).Error
}

func (s ShardStore) shardArchiveStats(shardID string) ([]ShardArchiveStat, error) {
	var rows []ShardArchiveStat
	err := s.db().Where("shard_id = ?", shardID).Find(&rows).Error
	return rows, err
}

// layoutSegment is the last directory of an archive glob that names its layout:
// k<minutes> (the first keyed layout) or sized, on a keyed archive's glob.
var layoutSegment = regexp.MustCompile(`/(k[1-9][0-9]*|sized)$`)

// rewriteDestPath derives a fresh parquet key next to the source, versioned by
// the rewrite instant so a retry never collides with the object a previous
// attempt may have written. A keyed archive is rewritten as a sized one
// (x/s.attempt-N.r<ns>/sized/**/*.parquet), because one file would put every
// service back in every file. Any other archive, the single file (x/s.parquet)
// or the time-partitioned glob (x/s/**/*.parquet), is rewritten as a single file.
func rewriteDestPath(old string, now time.Time) string {
	if shard.IsKeyed(old) {
		base := layoutSegment.ReplaceAllString(strings.TrimSuffix(old, "/**/*.parquet"), "")
		base = strings.TrimSuffix(base, ".parquet")
		return fmt.Sprintf("%s.r%d/sized/**/*.parquet", base, now.UnixNano())
	}
	base := strings.TrimSuffix(old, "/**/*.parquet")
	base = strings.TrimSuffix(base, ".parquet")
	return fmt.Sprintf("%s.r%d.parquet", base, now.UnixNano())
}

// ObjectDeleter removes an archived parquet object. path is a single file
// or a hive glob (.../**/*.parquet); the implementation expands a glob to
// its underlying objects. Deleting an already-gone object must be a
// success so the GC sweep is idempotent across retries.
type ObjectDeleter interface {
	DeleteParquet(path string) error
}

// SweepTombstones deletes every parquet object whose grace window has
// elapsed, then drops its tombstone row. S3 delete precedes the row
// delete, so a crash between them leaves the tombstone for the next
// sweep to retry (re-deleting a gone object is a no-op) — never a dropped
// row pointing at a live object. Per-object failures are logged and left
// for retry; the sweep continues. Returns the count deleted.
func (s ShardStore) SweepTombstones(now time.Time, deleter ObjectDeleter) (int, error) {
	var tombs []ParquetTombstone
	if err := s.db().Where("delete_after < ?", now).Find(&tombs).Error; err != nil {
		return 0, err
	}
	deleted := 0
	for _, t := range tombs {
		if err := deleter.DeleteParquet(t.Path); err != nil {
			weftsdk.Infof(context.Background(), "janitor GC: delete %s (shard %s): %v", t.Path, t.ShardID, err)
			continue
		}
		if err := s.db().Delete(&ParquetTombstone{}, t.ID).Error; err != nil {
			weftsdk.Infof(context.Background(), "janitor GC: drop tombstone %d: %v", t.ID, err)
			continue
		}
		deleted++
	}
	return deleted, nil
}

// Janitor is the daily retention sweep: plan over archived shards, execute
// each disposition, then GC any tombstones past their grace. It holds only
// small collaborators (not the whole Provisioner) so it's testable with
// fakes; the coordinator wires Retire/Refresh to the live registry.
type Janitor struct {
	Store     ShardStore
	Compactor shard.Compactor
	Deleter   ObjectDeleter
	// Retire moves a fully-expired shard out of service (fires the SM's
	// GONE trigger in the coordinator).
	Retire func(shardID string) error
	// Refresh repoints an archived shard's in-memory registry entry at its
	// rewritten parquet, so new query descriptors read the new file rather
	// than the soon-to-be-GC'd old one.
	Refresh func(shardID, newPath string)
	Grace   time.Duration
	GCDelay time.Duration
	Clock   func() time.Time
	// Interval is the sweep cadence (24h in production).
	Interval time.Duration
	// IsLeader gates the pass to one coordinator: every coordinator runs a
	// Janitor, and two passes at once would rewrite the same archives twice.
	// Nil means always run (tests, a single process).
	IsLeader func() bool
}

// NewProvisionerJanitor wires a Janitor to a live provisioner. Retire uses
// DeleteArchived with force=true — the janitor's per-customer expiry
// supersedes DeleteArchived's coarser bucket-tier retention guard. Refresh
// repoints the in-memory registry entry after a rewrite. Grace, GCDelay,
// and the clock take the production defaults.
func NewProvisionerJanitor(prov *Provisioner, compactor shard.Compactor, deleter ObjectDeleter, interval time.Duration) *Janitor {
	lambdaURL := prov.config.LambdaFunctionURL
	return &Janitor{
		Store:     ShardStore{prov.store},
		Compactor: compactor,
		Deleter:   deleter,
		Retire:    func(id string) error { return prov.DeleteArchived(id, true) },
		Refresh:   func(id, p string) { prov.registry.RefreshArchivedPath(id, p, lambdaURL) },
		Grace:     RetentionGrace,
		GCDelay:   RetentionGCDelay,
		Clock:     time.Now,
		Interval:  interval,
		IsLeader:  prov.IsLeader,
	}
}

// RunOnce executes a single janitor pass: classify every archived shard,
// purge the ones that need it, then GC due tombstones. Per-shard errors
// are logged and don't abort the pass.
func (j *Janitor) RunOnce(ctx context.Context) error {
	if j.IsLeader != nil && !j.IsLeader() {
		return nil
	}
	now := j.Clock()
	tiers, err := j.Store.ActiveCustomerTiers()
	if err != nil {
		return fmt.Errorf("janitor: load tiers: %w", err)
	}
	resolver := NewRetentionResolver(tiers)
	plan, err := j.Store.PlanRetention(now, j.Grace)
	if err != nil {
		return fmt.Errorf("janitor: plan: %w", err)
	}
	deps := PurgeDeps{
		Compactor: j.Compactor,
		Grace:     j.Grace,
		GCDelay:   j.GCDelay,
		Now:       j.Clock,
		Retire:    j.Retire,
	}
	for _, p := range plan {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := j.Store.ExecutePurge(p, resolver, deps)
		if err != nil {
			weftsdk.Infof(ctx, "janitor: purge %s (%s): %v", p.ShardID, p.Disposition, err)
			continue
		}
		if res.Outcome == OutcomeRewritten && j.Refresh != nil {
			j.Refresh(p.ShardID, res.NewPath)
		}
	}
	if _, err := j.Store.SweepTombstones(now, j.Deleter); err != nil {
		weftsdk.Infof(ctx, "janitor: GC sweep: %v", err)
	}
	return nil
}

// Start runs RunOnce on Interval until ctx is cancelled. The first pass
// waits one interval — retention isn't urgent and we'd rather not pile
// onto coordinator boot.
func (j *Janitor) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(j.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := j.RunOnce(ctx); err != nil {
					weftsdk.Infof(ctx, "janitor: run: %v", err)
				}
			}
		}
	}()
}
