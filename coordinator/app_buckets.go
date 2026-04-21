package coordinator

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kevlarcon/weft/store"
)

// AppBucket groups apps that share a shard fleet. ID is the surrogate
// FK target that shard_records.bucket_id points at; Label is the
// stable human-readable name (unique) code looks up by. Target is
// the desired number of INGESTING shards; Tier is the instance class.
// WindowDuration and RetentionPeriod govern rotation and archive
// timing per-bucket.
type AppBucket struct {
	ID              int64
	Label           string
	Apps            []string
	Target          int
	Tier            string
	WindowDuration  time.Duration
	RetentionPeriod time.Duration
	CreatedAt       string
}

// BucketStore is the subset of *store.Store that BucketManager needs.
// Extracted so tests can swap in an in-memory fake.
type BucketStore interface {
	ListBuckets() ([]store.Bucket, error)
	GetBucket(id int64) (store.Bucket, bool, error)
	GetBucketByLabel(label string) (store.Bucket, bool, error)
	SaveBucket(b *store.Bucket) error
}

// Buckets is the interface the provisioner depends on for bucket
// lookup, sizing, and split decisions. All reads go to the store
// on every call — no in-memory cache. That keeps callers from
// working off pointers whose fields have silently diverged from
// the DB after another code path mutated the row.
type Buckets interface {
	Default() (*AppBucket, error)
	ByID(id int64) (*AppBucket, error)
	ByLabel(label string) (*AppBucket, error)
	All() ([]*AppBucket, error)
	EvaluateSplit(stats map[string]int64) (string, error)
	Split(app string) (*AppBucket, error)
	AddAppToBucket(id int64, app string) (*AppBucket, error)
	// SetStore wires the manager to a live store. Called from the
	// coordinator boot path once store.Open completes; tests can
	// pass a fake. Implementations that don't need a store (the
	// stub in app_buckets_test.go) may no-op.
	SetStore(s BucketStore)
}

// BucketManager is a thin store wrapper. It holds no bucket data
// between calls; every method does a fresh DB read/write. The
// "manager" name is retained for continuity with the interface —
// functionally it's a service object.
type BucketManager struct {
	store BucketStore
	// SplitThreshold: if one app accounts for this fraction of a
	// shared bucket's rows, split it into a dedicated bucket.
	// Default 0.6 (60%).
	SplitThreshold float64
}

// NewBucketManager returns a manager wired to the given store.
// Pass nil for a tests-only manager that returns nil from every
// lookup and errors from every write (provided the test doesn't
// drive those paths).
func NewBucketManager(store BucketStore) *BucketManager {
	return &BucketManager{
		store:          store,
		SplitThreshold: 0.6,
	}
}

// SetStore swaps the backing store. Used by the coordinator boot
// sequence where the provisioner is constructed before store.Open
// completes; once store is ready, main.go wires it in.
func (bm *BucketManager) SetStore(s BucketStore) {
	bm.store = s
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
func (bm *BucketManager) EvaluateSplit(stats map[string]int64) (string, error) {
	buckets, err := bm.All()
	if err != nil {
		return "", err
	}
	for _, bucket := range buckets {
		if len(bucket.Apps) <= 1 {
			continue
		}
		var total int64
		counts := make(map[string]int64, len(bucket.Apps))
		for _, app := range bucket.Apps {
			c := stats[app]
			counts[app] = c
			total += c
		}
		if total == 0 {
			continue
		}
		for app, c := range counts {
			if float64(c)/float64(total) > bm.SplitThreshold {
				return app, nil
			}
		}
	}
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
		RetentionPeriod: src.RetentionPeriod,
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
	appsJSON, err := json.Marshal(b.Apps)
	if err != nil {
		return fmt.Errorf("marshal apps: %w", err)
	}
	rec := &store.Bucket{
		ID:                   b.ID,
		Label:                b.Label,
		Apps:                 string(appsJSON),
		TargetIngestingCount: b.Target,
		Tier:                 b.Tier,
		WindowDuration:       b.WindowDuration.String(),
		RetentionPeriod:      b.RetentionPeriod.String(),
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
func recordToAppBucket(r store.Bucket) *AppBucket {
	var apps []string
	if r.Apps != "" && r.Apps != "null" {
		_ = json.Unmarshal([]byte(r.Apps), &apps)
	}
	var winDur, retDur time.Duration
	if r.WindowDuration != "" {
		if d, err := time.ParseDuration(r.WindowDuration); err == nil {
			winDur = d
		} else {
			log.Printf("bucket-manager: bucket id=%d label=%s unparseable window_duration %q: %v",
				r.ID, r.Label, r.WindowDuration, err)
		}
	}
	if r.RetentionPeriod != "" {
		if d, err := time.ParseDuration(r.RetentionPeriod); err == nil {
			retDur = d
		} else {
			log.Printf("bucket-manager: bucket id=%d label=%s unparseable retention_period %q: %v",
				r.ID, r.Label, r.RetentionPeriod, err)
		}
	}
	return &AppBucket{
		ID:              r.ID,
		Label:           r.Label,
		Apps:            apps,
		Target:          r.TargetIngestingCount,
		Tier:            r.Tier,
		WindowDuration:  winDur,
		RetentionPeriod: retDur,
		CreatedAt:       r.CreatedAt,
	}
}
