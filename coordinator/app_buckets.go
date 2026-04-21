package coordinator

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kevlarcon/weft/store"
)

// AppBucket groups apps that share a shard fleet. Low-throughput apps
// share a bucket; high-throughput apps get dedicated buckets. Target
// is the desired number of INGESTING shards in the bucket; Tier is
// the instance class (which ASG, in prod) those shards run on.
// WindowDuration and RetentionPeriod govern rotation and archive
// timing per-bucket — zero means "fall back to provisioner config."
type AppBucket struct {
	ID              string
	Apps            []string
	Target          int
	Tier            string
	WindowDuration  time.Duration
	RetentionPeriod time.Duration
	// CreatedAt is round-tripped through LoadFromStore so SaveBucket
	// (via GORM's upsert) preserves the original timestamp on
	// mutation. Empty means "never loaded from store" — the in-memory
	// stub case. SaveBucket won't be called with an empty CreatedAt.
	CreatedAt string
}

// BucketStore is the subset of *store.Store that BucketManager needs.
// Extracted so tests can swap in an in-memory fake.
type BucketStore interface {
	ListBuckets() ([]store.Bucket, error)
	SaveBucket(b *store.Bucket) error
}

// Buckets is the interface the provisioner depends on for bucket
// lookup, sizing, and split decisions. Production passes a
// *BucketManager hydrated from the DB; tests can pass a stub
// (stubBuckets in app_buckets_test.go) to control which bucket
// christen() picks up without standing up a fake store.
type Buckets interface {
	Default() *AppBucket
	All() []*AppBucket
	EvaluateSplit(stats map[string]int64) string
	Split(app string) *AppBucket
	LoadFromStore(BucketStore) error
	// AddAppToBucket appends an app to the named bucket and persists.
	// Idempotent; returns the updated bucket snapshot so callers can
	// fan the new app out to INGESTING shards.
	AddAppToBucket(bucketID, app string) (*AppBucket, error)
}

// BucketManager tracks app-to-bucket assignments and splits/merges
// based on observed throughput.
type BucketManager struct {
	mu      sync.RWMutex
	buckets []*AppBucket
	store   BucketStore
	// SplitThreshold: if one app accounts for this fraction of a shared
	// shard's rows, split it into a dedicated bucket. Default 0.6 (60%).
	SplitThreshold float64
}

// NewBucketManager creates a manager seeded with the given apps in a
// single "default" bucket. Empty apps means an empty manager — the
// production wiring then calls LoadFromStore to hydrate from the
// buckets table (which migration 020 always seeds with the default
// catch-all row).
//
// "default" is the canonical name for any bucket the manager creates
// at construction; there's no separate "shared" naming.
func NewBucketManager(apps []string) *BucketManager {
	bm := &BucketManager{
		SplitThreshold: 0.6,
	}
	if len(apps) > 0 {
		bm.buckets = []*AppBucket{{
			ID:   "default",
			Apps: apps,
			Tier: "small",
		}}
	}
	return bm
}

// SetStore enables persistence for future Split/Add operations and
// retains the handle for ad-hoc reloads.
func (bm *BucketManager) SetStore(s BucketStore) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.store = s
}

// LoadFromStore replaces the manager's in-memory bucket set with the
// rows from the DB. Returns an error only on store failure; an empty
// store is a soft "nothing to load" (the caller may want to seed a
// default bucket explicitly).
func (bm *BucketManager) LoadFromStore(s BucketStore) error {
	recs, err := s.ListBuckets()
	if err != nil {
		return err
	}
	loaded := make([]*AppBucket, 0, len(recs))
	for _, r := range recs {
		var apps []string
		if r.Apps != "" && r.Apps != "null" {
			_ = json.Unmarshal([]byte(r.Apps), &apps)
		}
		// Durations default to zero on parse failure so the Go layer
		// can fall back to provisioner config. Missing values (NULL)
		// are already excluded by the NOT NULL column constraint.
		var winDur, retDur time.Duration
		if r.WindowDuration != "" {
			if d, err := time.ParseDuration(r.WindowDuration); err == nil {
				winDur = d
			} else {
				log.Printf("bucket-manager: bucket %s has unparseable window_duration %q: %v",
					r.ID, r.WindowDuration, err)
			}
		}
		if r.RetentionPeriod != "" {
			if d, err := time.ParseDuration(r.RetentionPeriod); err == nil {
				retDur = d
			} else {
				log.Printf("bucket-manager: bucket %s has unparseable retention_period %q: %v",
					r.ID, r.RetentionPeriod, err)
			}
		}
		loaded = append(loaded, &AppBucket{
			ID:              r.ID,
			Apps:            apps,
			Target:          r.TargetIngestingCount,
			Tier:            r.Tier,
			WindowDuration:  winDur,
			RetentionPeriod: retDur,
			CreatedAt:       r.CreatedAt,
		})
	}

	bm.mu.Lock()
	bm.buckets = loaded
	bm.store = s
	bm.mu.Unlock()
	log.Printf("bucket-manager: loaded %d buckets from store", len(loaded))
	return nil
}

// Default returns the catch-all bucket (apps == []) — the bucket that
// new apps fall into until split-evaluation moves them out. Returns
// nil if no bucket has an empty Apps list, which is a misconfigured
// state worth logging at the call site.
func (bm *BucketManager) Default() *AppBucket {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	for _, b := range bm.buckets {
		if len(b.Apps) == 0 {
			return b
		}
	}
	return nil
}

// bucketToStoreRecord converts an in-memory bucket snapshot to the
// store's Bucket shape for SaveBucket. CreatedAt is round-tripped
// from LoadFromStore so GORM's Save (UPDATE on PK match) preserves
// the original timestamp.
func bucketToStoreRecord(b *AppBucket, appsJSON []byte) *store.Bucket {
	return &store.Bucket{
		ID:                   b.ID,
		Apps:                 string(appsJSON),
		TargetIngestingCount: b.Target,
		Tier:                 b.Tier,
		WindowDuration:       b.WindowDuration.String(),
		RetentionPeriod:      b.RetentionPeriod.String(),
		CreatedAt:            b.CreatedAt,
	}
}

// All returns a snapshot of current buckets.
func (bm *BucketManager) All() []*AppBucket {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	out := make([]*AppBucket, len(bm.buckets))
	copy(out, bm.buckets)
	return out
}

// BucketFor returns the bucket that contains the given app, or nil.
func (bm *BucketManager) BucketFor(app string) *AppBucket {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	for _, b := range bm.buckets {
		for _, a := range b.Apps {
			if a == app {
				return b
			}
		}
	}
	return nil
}

// EvaluateSplit checks if any app in a shared bucket should be split out
// based on the per-app row counts from the last monitoring window.
// Returns the app name that should be split, or "" if no split needed.
func (bm *BucketManager) EvaluateSplit(stats map[string]int64) string {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	for _, bucket := range bm.buckets {
		if len(bucket.Apps) <= 1 {
			continue // already dedicated
		}

		// Calculate total and per-app counts for this bucket.
		var total int64
		appCounts := make(map[string]int64)
		for _, app := range bucket.Apps {
			c := stats[app]
			appCounts[app] = c
			total += c
		}

		if total == 0 {
			continue
		}

		// Check if any app dominates.
		for app, count := range appCounts {
			if float64(count)/float64(total) > bm.SplitThreshold {
				return app
			}
		}
	}
	return ""
}

// Split moves an app from its shared bucket into a new dedicated bucket.
// Returns the new bucket, or nil if the app wasn't in a shared bucket.
func (bm *BucketManager) Split(app string) *AppBucket {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	for _, bucket := range bm.buckets {
		if len(bucket.Apps) <= 1 {
			continue
		}
		for i, a := range bucket.Apps {
			if a == app {
				// Remove from shared bucket.
				bucket.Apps = append(bucket.Apps[:i], bucket.Apps[i+1:]...)

				// Create dedicated bucket.
				newBucket := &AppBucket{
					ID:   "dedicated-" + app,
					Apps: []string{app},
				}
				bm.buckets = append(bm.buckets, newBucket)
				log.Printf("bucket-manager: split %s into dedicated bucket (shared now has %d apps)", app, len(bucket.Apps))
				return newBucket
			}
		}
	}
	return nil
}

// AddAppToBucket appends an app to the named bucket and persists the
// mutation. Idempotent — if the app is already in the bucket, the
// call is a no-op (no save, no error). Returns the bucket snapshot
// so callers can fan the new app out to INGESTING shards.
//
// When the store is configured, this is the only sanctioned way to
// mutate a bucket's apps list; all other code should read bucket.
// Apps but never write it directly. That keeps the DB and in-memory
// views in sync.
func (bm *BucketManager) AddAppToBucket(bucketID, app string) (*AppBucket, error) {
	bm.mu.Lock()
	var target *AppBucket
	for _, b := range bm.buckets {
		if b.ID == bucketID {
			target = b
			break
		}
	}
	if target == nil {
		bm.mu.Unlock()
		return nil, fmt.Errorf("bucket %q not found", bucketID)
	}
	for _, a := range target.Apps {
		if a == app {
			bm.mu.Unlock()
			return target, nil // idempotent
		}
	}
	target.Apps = append(target.Apps, app)
	snapshot := &AppBucket{
		ID:              target.ID,
		Apps:            append([]string(nil), target.Apps...),
		Target:          target.Target,
		Tier:            target.Tier,
		WindowDuration:  target.WindowDuration,
		RetentionPeriod: target.RetentionPeriod,
	}
	store := bm.store
	bm.mu.Unlock()

	if store == nil {
		return snapshot, nil
	}
	appsJSON, _ := json.Marshal(snapshot.Apps)
	rec := bucketToStoreRecord(snapshot, appsJSON)
	if err := store.SaveBucket(rec); err != nil {
		return nil, fmt.Errorf("persist bucket %s: %w", bucketID, err)
	}
	return snapshot, nil
}
