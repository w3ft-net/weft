package coordinator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kevlarcon/weft/shard"
)

// ProvisionerConfig controls shard rotation thresholds.
type ProvisionerConfig struct {
	MaxRows           int64         // rotate when row count exceeds this (default 5M)
	MaxDiskBytes      int64         // rotate when disk exceeds this (default 500MB)
	MaxDuration       time.Duration // rotate after this time (default 8h)
	PollInterval      time.Duration // how often to check (default 5min)
	IngestingPerBucket int          // number of concurrent INGESTING shards per app bucket (default 2)
}

// DefaultProvisionerConfig returns sensible production defaults.
func DefaultProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		MaxRows:            5_000_000,
		MaxDiskBytes:       500 * 1024 * 1024, // 500MB
		MaxDuration:        8 * time.Hour,
		PollInterval:       5 * time.Minute,
		IngestingPerBucket: 2,
	}
}

// DevProvisionerConfig returns faster thresholds for development/testing.
func DevProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		MaxRows:            50_000,
		MaxDiskBytes:       50 * 1024 * 1024, // 50MB
		MaxDuration:        1 * time.Hour,
		PollInterval:       5 * time.Second,
		IngestingPerBucket: 2,
	}
}

// AppStats tracks per-app ingestion rates for adaptive bucket splitting.
type AppStats struct {
	mu     sync.Mutex
	counts map[string]*atomic.Int64
}

// NewAppStats creates a new stats tracker.
func NewAppStats() *AppStats {
	return &AppStats{
		counts: make(map[string]*atomic.Int64),
	}
}

// Record increments the row count for an app.
func (s *AppStats) Record(app string, n int64) {
	s.mu.Lock()
	counter, ok := s.counts[app]
	if !ok {
		counter = &atomic.Int64{}
		s.counts[app] = counter
	}
	s.mu.Unlock()
	counter.Add(n)
}

// Snapshot returns current counts and resets them.
func (s *AppStats) Snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.counts))
	for app, counter := range s.counts {
		out[app] = counter.Swap(0)
	}
	return out
}

// Provisioner monitors shard fill rates and handles rotation.
type Provisioner struct {
	registry *ShardRegistry
	launcher ShardLauncher
	config   ProvisionerConfig
	stats    *AppStats
	Buckets  *BucketManager
}

// NewProvisioner creates a provisioner.
func NewProvisioner(registry *ShardRegistry, launcher ShardLauncher, config ProvisionerConfig) *Provisioner {
	return &Provisioner{
		registry: registry,
		launcher: launcher,
		config:   config,
		stats:    NewAppStats(),
		Buckets:  NewBucketManager(nil),
	}
}

// Stats returns the provisioner's app stats tracker. The drain loop
// should call stats.Record() after each successful ingestion.
func (p *Provisioner) Stats() *AppStats {
	return p.stats
}

// Start runs the provisioner loop until the context is cancelled.
func (p *Provisioner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(p.config.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.check(ctx)
			}
		}
	}()
}

// check evaluates each INGESTING shard for rotation and app split.
func (p *Provisioner) check(ctx context.Context) {
	// Evaluate bucket splits based on ingestion stats.
	if p.Buckets != nil {
		snapshot := p.stats.Snapshot()
		if app := p.Buckets.EvaluateSplit(snapshot); app != "" {
			if bucket := p.Buckets.Split(app); bucket != nil {
				// Provision a dedicated shard for the split-out app.
				log.Printf("provisioner: splitting %s into dedicated shard", app)
				p.provisionForBucket(ctx, bucket)
			}
		}
	}

	// Ensure each bucket has enough INGESTING shards.
	if p.Buckets != nil && p.config.IngestingPerBucket > 0 {
		for _, bucket := range p.Buckets.Buckets() {
			app := bucket.Apps[0] // check using first app in bucket
			current := p.registry.IngestingCount(app)
			for current < p.config.IngestingPerBucket {
				log.Printf("provisioner: scaling up %s (%d/%d ingesting shards)",
					bucket.ID, current, p.config.IngestingPerBucket)
				p.provisionForBucket(ctx, bucket)
				current++
			}
		}
	}

	for _, entry := range p.registry.Entries() {
		if entry.State != StateIngesting {
			continue
		}

		shouldRotate := false
		reason := ""

		if p.config.MaxRows > 0 && entry.RowCount > p.config.MaxRows {
			shouldRotate = true
			reason = fmt.Sprintf("row count %d > %d", entry.RowCount, p.config.MaxRows)
		}

		if !shouldRotate && p.config.MaxDuration > 0 && time.Since(entry.CreatedAt) > p.config.MaxDuration {
			shouldRotate = true
			reason = fmt.Sprintf("age %v > %v", time.Since(entry.CreatedAt).Round(time.Second), p.config.MaxDuration)
		}

		if shouldRotate {
			log.Printf("provisioner: rotating shard %s (%s)", entry.ID, reason)
			p.rotate(ctx, entry)
		}
	}
}

// rotate transitions an INGESTING shard to READ_ONLY and launches a replacement.
func (p *Provisioner) rotate(ctx context.Context, old *ShardEntry) {
	// Launch a new shard instance.
	tier := "small" // TODO: determine from app bucket throughput
	addr, err := p.launcher.Launch(ctx, tier)
	if err != nil {
		log.Printf("provisioner: failed to launch replacement for %s: %v", old.ID, err)
		return
	}

	// Connect to the new shard.
	now := time.Now().UTC()
	seq := p.registry.Count()
	newID := fmt.Sprintf("shard-%s-%s-%d", joinApps(old.Apps), now.Format("20060102T1504"), seq)

	gs, err := shard.NewGRPCShard(newID, addr, now, now.Add(100*365*24*time.Hour))
	if err != nil {
		log.Printf("provisioner: failed to connect to new shard at %s: %v", addr, err)
		p.launcher.Terminate(ctx, addr)
		return
	}

	// Register the new shard.
	p.registry.Add(&ShardEntry{
		ID:          newID,
		Apps:        old.Apps,
		WindowStart: now,
		WindowEnd:   now.Add(100 * 365 * 24 * time.Hour), // far future until next rotation
		State:       StateIngesting,
		Addr:        addr,
		Shard:       gs,
		CreatedAt:   now,
	})

	// Transition old shard.
	old.WindowEnd = now
	old.State = StateReadOnly
	log.Printf("provisioner: shard %s → READ_ONLY, new shard %s at %s", old.ID, newID, addr)
}

// provisionForBucket launches a new INGESTING shard for a bucket.
func (p *Provisioner) provisionForBucket(ctx context.Context, bucket *AppBucket) {
	tier := "small" // TODO: determine from throughput history
	addr, err := p.launcher.Launch(ctx, tier)
	if err != nil {
		log.Printf("provisioner: failed to launch shard for bucket %s: %v", bucket.ID, err)
		return
	}

	now := time.Now().UTC()
	seq := p.registry.Count() // unique suffix
	id := fmt.Sprintf("shard-%s-%s-%d", joinApps(bucket.Apps), now.Format("20060102T1504"), seq)

	gs, err := shard.NewGRPCShard(id, addr, now, now.Add(100*365*24*time.Hour))
	if err != nil {
		log.Printf("provisioner: failed to connect to new shard at %s: %v", addr, err)
		p.launcher.Terminate(ctx, addr)
		return
	}

	p.registry.Add(&ShardEntry{
		ID:          id,
		Apps:        bucket.Apps,
		WindowStart: now,
		WindowEnd:   now.Add(100 * 365 * 24 * time.Hour),
		State:       StateIngesting,
		Addr:        addr,
		Shard:       gs,
		CreatedAt:   now,
	})

	log.Printf("provisioner: launched shard %s for bucket %s at %s", id, bucket.ID, addr)
}

func joinApps(apps []string) string {
	if len(apps) == 0 {
		return "shared"
	}
	if len(apps) == 1 {
		return apps[0]
	}
	return fmt.Sprintf("%d-apps", len(apps))
}
