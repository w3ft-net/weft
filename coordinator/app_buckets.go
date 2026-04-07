package coordinator

import (
	"log"
	"sync"
)

// AppBucket groups apps that share a shard chain. Low-throughput apps
// share a bucket; high-throughput apps get dedicated buckets.
type AppBucket struct {
	ID   string
	Apps []string
}

// BucketManager tracks app-to-bucket assignments and splits/merges
// based on observed throughput.
type BucketManager struct {
	mu      sync.RWMutex
	buckets []*AppBucket
	// SplitThreshold: if one app accounts for this fraction of a shared
	// shard's rows, split it into a dedicated bucket. Default 0.6 (60%).
	SplitThreshold float64
}

// NewBucketManager creates a manager with all known apps in a single shared bucket.
func NewBucketManager(apps []string) *BucketManager {
	bm := &BucketManager{
		SplitThreshold: 0.6,
	}
	if len(apps) > 0 {
		bm.buckets = []*AppBucket{{
			ID:   "shared",
			Apps: apps,
		}}
	}
	return bm
}

// Buckets returns a snapshot of current buckets.
func (bm *BucketManager) Buckets() []*AppBucket {
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

// AddApp adds an app to the shared bucket. Used when new apps are discovered.
func (bm *BucketManager) AddApp(app string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	// Check if already tracked.
	for _, b := range bm.buckets {
		for _, a := range b.Apps {
			if a == app {
				return
			}
		}
	}

	// Add to shared bucket, creating one if needed.
	if len(bm.buckets) == 0 {
		bm.buckets = append(bm.buckets, &AppBucket{ID: "shared", Apps: []string{app}})
	} else {
		// Add to the first shared (multi-app) bucket, or the first bucket.
		for _, b := range bm.buckets {
			if len(b.Apps) > 1 || b.ID == "shared" {
				b.Apps = append(b.Apps, app)
				return
			}
		}
		bm.buckets[0].Apps = append(bm.buckets[0].Apps, app)
	}
}
