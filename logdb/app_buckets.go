package logdb

import (
	"fmt"
	"log"
	"time"

	"github.com/kevlarcon/weft/store"
)

// AppBucket groups apps that share a shard fleet. ID is the surrogate
// FK target that shard_records.bucket_id points at; Label is the
// stable human-readable name (unique) code looks up by. Target is
// the desired number of INGESTING shards; Tier is the instance class.
// WindowDuration governs both rotation (INGESTING→READ_ONLY at
// window_end) and archive timing (READ_ONLY→ARCHIVED after the
// same duration past window_end).
type AppBucket struct {
	ID             int64
	Label          string
	Apps           []string
	Target         int
	Tier           string
	WindowDuration time.Duration
	CreatedAt      string
}

// BucketManager is a thin *store.Store wrapper. It holds no bucket
// data between calls; every method does a fresh DB read/write, so
// callers never work off a pointer whose fields have silently
// diverged from the row after another path mutated it. The "manager"
// name is historical — functionally it's a service object over the
// buckets table.
type BucketManager struct {
	store *store.Store
	// SplitThreshold: if one app accounts for this fraction of a
	// shared bucket's rows, split it into a dedicated bucket.
	// Default 0.6 (60%).
	SplitThreshold float64
}

// NewBucketManager returns a manager wired to the given store. Pass
// nil only in tests that never drive a bucket path; production wires
// a real store via Provisioner.SetStore.
func NewBucketManager(s *store.Store) *BucketManager {
	return &BucketManager{
		store:          s,
		SplitThreshold: 0.6,
	}
}

// Default returns the bucket with label "default". No memoization;
// every call does a ByLabel lookup. Errors propagate; nil result
// with nil error means "not found" (operator hasn't seeded the
// default bucket yet).
func (bm *BucketManager) Default() (*AppBucket, error) {
	return bm.ByLabel("default")
}

// ByID fetches a bucket by surrogate primary key. Returns (nil, nil)
// when not found so callers can distinguish "no record" from "DB
// error."
func (bm *BucketManager) ByID(id int64) (*AppBucket, error) {
	if bm.store == nil {
		return nil, nil
	}
	rec, ok, err := bm.store.GetBucket(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return recordToAppBucket(rec), nil
}

// ByLabel fetches a bucket by its unique label.
func (bm *BucketManager) ByLabel(label string) (*AppBucket, error) {
	if bm.store == nil {
		return nil, nil
	}
	rec, ok, err := bm.store.GetBucketByLabel(label)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return recordToAppBucket(rec), nil
}

// All returns every bucket. Always fresh from store.
func (bm *BucketManager) All() ([]*AppBucket, error) {
	if bm.store == nil {
		return nil, nil
	}
	recs, err := bm.store.ListBuckets()
	if err != nil {
		return nil, err
	}
	out := make([]*AppBucket, 0, len(recs))
	for _, r := range recs {
		out = append(out, recordToAppBucket(r))
	}
	return out, nil
}

// EvaluateSplit consults ingestion stats and returns the first app
// that dominates a multi-app bucket beyond SplitThreshold. Returns
// "" when no split is warranted.
//
// TODO(split): currently disabled — see docs/plans/bucket-split-policy.md.
// The ratio-only predicate cascades: splitting a dominant app leaves
// the tail in default, where a different app then dominates the
// remaining volume and triggers another split. Combined with Split
// inheriting the source's Target (not moving capacity), every
// dedicated bucket adds bucket.Target to desired_capacity, producing
// the 2026-04-22 runaway (10 small-tier shards against target=2).
// Re-enable once Split moves capacity, EvaluateSplit skips apps that
// already own a dedicated bucket, and there's a cooldown/budget.
func (bm *BucketManager) EvaluateSplit(_ map[string]int64) (string, error) {
	return "", nil
}

// Split moves an app out of its current (shared) bucket into a
// newly-minted dedicated bucket. Inherits target/tier/window/
// retention from the source bucket. Persists both the source
// (app removed) and the new dedicated bucket.
func (bm *BucketManager) Split(app string) (*AppBucket, error) {
	if bm.store == nil {
		return nil, fmt.Errorf("split %q: no store configured", app)
	}
	buckets, err := bm.All()
	if err != nil {
		return nil, err
	}

	var src *AppBucket
	for _, b := range buckets {
		if len(b.Apps) <= 1 {
			continue
		}
		for _, a := range b.Apps {
			if a == app {
				src = b
				break
			}
		}
		if src != nil {
			break
		}
	}
	if src == nil {
		return nil, nil
	}

	// Remove app from source, persist.
	newSrcApps := make([]string, 0, len(src.Apps)-1)
	for _, a := range src.Apps {
		if a != app {
			newSrcApps = append(newSrcApps, a)
		}
	}
	src.Apps = newSrcApps
	if err := bm.save(src); err != nil {
		return nil, fmt.Errorf("persist source bucket after split: %w", err)
	}

	// Create the dedicated bucket — ID auto-assigned on INSERT.
	dedicated := &AppBucket{
		Label:           "dedicated-" + app,
		Apps:            []string{app},
		Target:          src.Target,
		Tier:            src.Tier,
		WindowDuration:  src.WindowDuration,
	}
	if err := bm.save(dedicated); err != nil {
		return nil, fmt.Errorf("persist dedicated bucket: %w", err)
	}
	log.Printf("bucket-manager: split %q out of bucket %s (%d), created dedicated id=%d",
		app, src.Label, src.ID, dedicated.ID)
	return dedicated, nil
}

// AddAppToBucket appends an app to the bucket with the given
// surrogate id and persists. Idempotent — if the app is already in
// the bucket, no-op, no write.
func (bm *BucketManager) AddAppToBucket(id int64, app string) (*AppBucket, error) {
	if bm.store == nil {
		return nil, fmt.Errorf("add app %q to bucket %d: no store", app, id)
	}
	bucket, err := bm.ByID(id)
	if err != nil {
		return nil, err
	}
	if bucket == nil {
		return nil, fmt.Errorf("bucket id=%d not found", id)
	}
	for _, a := range bucket.Apps {
		if a == app {
			return bucket, nil
		}
	}
	bucket.Apps = append(bucket.Apps, app)
	if err := bm.save(bucket); err != nil {
		return nil, err
	}
	return bucket, nil
}

// save is the internal persistence helper — marshals the in-memory
// AppBucket back into a store.Bucket record and calls SaveBucket.
// After insert (ID was 0), copies the new surrogate back onto the
// caller's pointer.
func (bm *BucketManager) save(b *AppBucket) error {
	rec := &store.Bucket{
		ID:                   b.ID,
		Label:                b.Label,
		Apps:                 b.Apps,
		TargetIngestingCount: b.Target,
		Tier:                 b.Tier,
		WindowDuration:       b.WindowDuration.String(),
		CreatedAt:            b.CreatedAt,
	}
	if err := bm.store.SaveBucket(rec); err != nil {
		return err
	}
	b.ID = rec.ID
	return nil
}

// recordToAppBucket converts a store record to the in-memory shape.
// Duration strings default to zero on parse failure; callers that
// need a non-zero duration should supply a provisioner-config
// fallback (see provisioner.windowDurationFor).
// windowDurationOr returns the bucket's WindowDuration or a
// sensible fallback. Used by CurrentWindow/NextWindow when a
// bucket hasn't been hydrated with a real duration yet.
func (b *AppBucket) windowDurationOr(fallback time.Duration) time.Duration {
	if b.WindowDuration > 0 {
		return b.WindowDuration
	}
	return fallback
}

// CurrentWindow returns the [start, end) window bounds that the
// bucket's shards should be writing to at `now`. Aligned to the
// bucket's WindowDuration via Truncate.
func (b *AppBucket) CurrentWindow(now time.Time) (start, end time.Time) {
	dur := b.windowDurationOr(7 * 24 * time.Hour)
	start = now.UTC().Truncate(dur)
	return start, start.Add(dur)
}

// NextWindow returns the [start, end) bounds of the window
// immediately after the current one — the window pre-warm fills.
func (b *AppBucket) NextWindow(now time.Time) (start, end time.Time) {
	_, currEnd := b.CurrentWindow(now)
	dur := b.windowDurationOr(7 * 24 * time.Hour)
	return currEnd, currEnd.Add(dur)
}

// ShouldPrewarmNextWindow reports whether the reconciler should
// mint next-window LAUNCHING rows this tick. The current policy is
// simple date math against a package-scoped PrewarmLead; the
// method exists on AppBucket so future strategies (ingestion burn
// rate, disk-fill slope, p99 latency tracker) can swap the policy
// without changing every caller.
//
// TODO: extend to consider per-shard ingest rate so bursty
// workloads pre-warm sooner than steady-state ones.
func (b *AppBucket) ShouldPrewarmNextWindow(now time.Time, ingesting []*ShardEntry) bool {
	threshold := now.Add(PrewarmLead)
	for _, e := range ingesting {
		if e.State != StateIngesting {
			continue
		}
		if e.WindowEnd.IsZero() {
			continue
		}
		if !threshold.Before(e.WindowEnd) {
			return true
		}
	}
	return false
}

func recordToAppBucket(r store.Bucket) *AppBucket {
	var winDur time.Duration
	if r.WindowDuration != "" {
		if d, err := time.ParseDuration(r.WindowDuration); err == nil {
			winDur = d
		} else {
			log.Printf("bucket-manager: bucket id=%d label=%s unparseable window_duration %q: %v",
				r.ID, r.Label, r.WindowDuration, err)
		}
	}
	return &AppBucket{
		ID:             r.ID,
		Label:          r.Label,
		Apps:           r.Apps,
		Target:         r.TargetIngestingCount,
		Tier:           r.Tier,
		WindowDuration: winDur,
		CreatedAt:      r.CreatedAt,
	}
}
