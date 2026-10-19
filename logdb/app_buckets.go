package logdb

import (
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// Bucket groups apps that share a shard fleet. ID is the surrogate
// FK target that shard_records.bucket_id points at; Label is the
// stable human-readable name (unique) code looks up by. Target is
// the desired number of INGESTING shards; Tier is the instance class.
// WindowDuration governs both rotation (INGESTING→READ_ONLY at
// window_end) and archive timing (READ_ONLY→ARCHIVED after the
// same duration past window_end).
type Bucket struct {
	ID             int64
	Label          string
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
func (bm *BucketManager) Default() (*Bucket, error) {
	return bm.ByLabel("default")
}

// ByID fetches a bucket by surrogate primary key. Returns (nil, nil)
// when not found so callers can distinguish "no record" from "DB
// error."
func (bm *BucketManager) ByID(id int64) (*Bucket, error) {
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
	return recordToBucket(rec), nil
}

// ByLabel fetches a bucket by its unique label.
func (bm *BucketManager) ByLabel(label string) (*Bucket, error) {
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
	return recordToBucket(rec), nil
}

// All returns every bucket. Always fresh from store.
func (bm *BucketManager) All() ([]*Bucket, error) {
	if bm.store == nil {
		return nil, nil
	}
	recs, err := bm.store.ListBuckets()
	if err != nil {
		return nil, err
	}
	out := make([]*Bucket, 0, len(recs))
	for _, r := range recs {
		out = append(out, recordToBucket(r))
	}
	return out, nil
}

// save is the internal persistence helper — marshals the in-memory
// Bucket back into a store.Bucket record and calls SaveBucket.
// After insert (ID was 0), copies the new surrogate back onto the
// caller's pointer.
func (bm *BucketManager) save(b *Bucket) error {
	rec := &store.Bucket{
		ID:                   b.ID,
		Label:                b.Label,
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

// recordToBucket converts a store record to the in-memory shape.
// Duration strings default to zero on parse failure; callers that
// need a non-zero duration should supply a provisioner-config
// fallback (see provisioner.windowDurationFor).
// windowDurationOr returns the bucket's WindowDuration or a
// sensible fallback. Used by CurrentWindow/NextWindow when a
// bucket hasn't been hydrated with a real duration yet.
func (b *Bucket) windowDurationOr(fallback time.Duration) time.Duration {
	if b.WindowDuration > 0 {
		return b.WindowDuration
	}
	return fallback
}

// CurrentWindow returns the [start, end) window bounds that the
// bucket's shards should be writing to at `now`. Aligned to the
// bucket's WindowDuration via Truncate.
func (b *Bucket) CurrentWindow(now time.Time) (start, end time.Time) {
	dur := b.windowDurationOr(7 * 24 * time.Hour)
	start = now.UTC().Truncate(dur)
	return start, start.Add(dur)
}

// NextWindow returns the [start, end) bounds of the window
// immediately after the current one — the window pre-warm fills.
func (b *Bucket) NextWindow(now time.Time) (start, end time.Time) {
	_, currEnd := b.CurrentWindow(now)
	dur := b.windowDurationOr(7 * 24 * time.Hour)
	return currEnd, currEnd.Add(dur)
}

func recordToBucket(r store.Bucket) *Bucket {
	var winDur time.Duration
	if r.WindowDuration != "" {
		if d, err := time.ParseDuration(r.WindowDuration); err == nil {
			winDur = d
		} else {
			weftsdk.Infof(context.Background(), "bucket-manager: bucket id=%d label=%s unparseable window_duration %q: %v",
				r.ID, r.Label, r.WindowDuration, err)
		}
	}
	return &Bucket{
		ID:             r.ID,
		Label:          r.Label,
		Target:         r.TargetIngestingCount,
		Tier:           r.Tier,
		WindowDuration: winDur,
		CreatedAt:      r.CreatedAt,
	}
}
