package coordinator

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// ProvisionerConfig controls shard rotation thresholds.
type ProvisionerConfig struct {
	MaxRows            int64           // rotate when row count exceeds this (default 5M)
	MaxDiskBytes       int64           // rotate when disk exceeds this (default 500MB)
	MaxDuration        time.Duration   // rotate after this time (default 8h)
	PollInterval       time.Duration   // how often to check (default 5min)
	IngestingPerBucket int             // number of concurrent INGESTING shards per app bucket (default 2)
	RetentionPeriod    time.Duration   // how long READ_ONLY shards stay hot before archiving (0 = no archival)
	ParquetDir         string          // directory for Parquet exports (local path or s3:// prefix)
	S3Config           *shard.S3Config // S3 credentials for Parquet export/read (nil = local only)
	LambdaFunctionURL  string          // Lambda function URL for cold queries (empty = in-process ColdShard)
	Store              *store.Store    // for reading shard heartbeats (nil = no heartbeat adoption)
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

// SetStore provides the store for heartbeat queries. Called after the store
// is opened (which may happen after the provisioner starts).
func (p *Provisioner) SetStore(s *store.Store) {
	p.config.Store = s
}

// Stats returns the provisioner's app stats tracker. The drain loop
// should call stats.Record() after each successful ingestion.
func (p *Provisioner) Stats() *AppStats {
	return p.stats
}

// Start runs the provisioner loop until the context is cancelled.
// It ensures at least one INGESTING shard exists on startup.
func (p *Provisioner) Start(ctx context.Context) {
	go func() {
		// First tick: adopt live shards from heartbeats, then bootstrap if needed.
		p.adoptFromHeartbeats()
		if p.registry.TotalIngesting() == 0 {
			log.Printf("provisioner: no ingesting shards after adoption, bootstrapping")
			p.bootstrap(ctx)
		}

		ticker := time.NewTicker(p.config.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.adoptFromHeartbeats()
				p.reapDeadShards()
				p.check(ctx)
			}
		}
	}()
}

// adoptFromHeartbeats reads recent shard heartbeats from the store
// and adds any unknown shards to the registry.
func (p *Provisioner) adoptFromHeartbeats() {
	if p.config.Store == nil {
		return
	}

	heartbeats, err := p.config.Store.LiveShardHeartbeats("2 minutes")
	if err != nil {
		log.Printf("provisioner: heartbeat query failed: %v", err)
		return
	}

	for _, hb := range heartbeats {
		existing := p.registry.Get(hb.ShardID)
		if existing != nil {
			if existing.Addr == hb.Addr {
				// Same shard, same address. Unsuspend if it was suspended.
				if existing.Suspended {
					log.Printf("provisioner: unsuspending shard %s (heartbeat received)", hb.ShardID)
					existing.Suspended = false
				}
				continue
			}
			// Same ID, different address → instance replaced.
			gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr)
			if err != nil {
				log.Printf("provisioner: reconnect %s at %s failed: %v", hb.ShardID, hb.Addr, err)
				continue
			}
			log.Printf("provisioner: shard %s moved %s → %s", hb.ShardID, existing.Addr, hb.Addr)
			existing.Shard.Close()
			existing.Shard = gs
			existing.Addr = hb.Addr
			existing.InstanceID = hb.InstanceID
			existing.Suspended = false
			continue
		}

		// New shard — adopt it.
		gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr)
		if err != nil {
			log.Printf("provisioner: adopt %s at %s failed: %v", hb.ShardID, hb.Addr, err)
			continue
		}

		state := StateIngesting
		if hb.State == "READ_ONLY" {
			state = StateReadOnly
		}

		p.registry.Add(&ShardEntry{
			ID:         hb.ShardID,
			Addr:       hb.Addr,
			InstanceID: hb.InstanceID,
			State:      state,
			RowCount:   hb.RowCount,
			Shard:      gs,
			CreatedAt:  time.Now(),
		})
		log.Printf("provisioner: adopted shard %s at %s (state=%s, rows=%d)",
			hb.ShardID, hb.Addr, hb.State, hb.RowCount)
	}
}

// reapDeadShards removes registry entries for shards that have stopped
// heartbeating. Skips archived shards (cold storage, no heartbeat expected)
// and skips if no heartbeats exist at all (startup race protection).
func (p *Provisioner) reapDeadShards() {
	if p.config.Store == nil {
		return
	}
	heartbeats, err := p.config.Store.LiveShardHeartbeats("2 minutes")
	if err != nil || len(heartbeats) == 0 {
		return // no data or no heartbeats yet — don't reap
	}

	live := make(map[string]bool, len(heartbeats))
	for _, hb := range heartbeats {
		live[hb.ShardID] = true
	}

	for _, entry := range p.registry.Entries() {
		if entry.State == StateArchived {
			continue
		}
		if live[entry.ID] {
			continue
		}
		log.Printf("provisioner: reaping dead shard %s (no heartbeat)", entry.ID)
		p.registry.Remove(entry.ID)
	}
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// bootstrap ensures at least one shard instance is launching.
// For ASG launchers, it checks whether instances already exist before
// scaling — avoids ratcheting up desired capacity on every coordinator restart.
// For local launchers, it uses launchAndConfigure for immediate setup.
func (p *Provisioner) bootstrap(ctx context.Context) {
	if asg, isASG := p.launcher.(*ASGLauncher); isASG {
		// Check if the ASG already has running instances — if so, just
		// wait for them to heartbeat rather than scaling up further.
		for _, asgName := range asg.tiers {
			ids, err := asg.asgInstanceIDs(ctx, asgName)
			if err == nil && len(ids) > 0 {
				log.Printf("provisioner: ASG %s already has %d instances, waiting for heartbeats", asgName, len(ids))
				return
			}
		}
		log.Printf("provisioner: no ASG instances found, launching one (will adopt via heartbeat)")
		if _, err := p.launcher.LaunchInstance(ctx, "small"); err != nil {
			log.Printf("provisioner: bootstrap ASG launch failed: %v", err)
		}
		return
	}

	// Local launcher: configure immediately.
	now := time.Now()
	windowEnd := now.Add(p.config.MaxDuration)
	if p.config.MaxDuration == 0 {
		windowEnd = now.Add(8 * time.Hour)
	}

	shardID := fmt.Sprintf("shard-%d", now.UnixMilli())
	entry, err := p.launchAndConfigure(ctx, shardID, "small", nil, now, windowEnd)
	if err != nil {
		log.Printf("provisioner: bootstrap failed: %v", err)
		return
	}

	p.registry.Add(entry)
	log.Printf("provisioner: bootstrapped shard %s (%s to %s)",
		shardID, now.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
}

// launchAndConfigure is the shared lifecycle logic: launch instance,
// connect via gRPC, call Configure RPC, return a registry entry.
// This runs the same code path for LocalLauncher and ASGLauncher.
func (p *Provisioner) launchAndConfigure(ctx context.Context, shardID, tier string, apps []string, windowStart, windowEnd time.Time) (*ShardEntry, error) {
	log.Printf("provisioner: launching shard %s (tier=%s)", shardID, tier)

	addr, err := p.launcher.LaunchInstance(ctx, tier)
	if err != nil {
		return nil, fmt.Errorf("launch: %w", err)
	}

	// Connect via gRPC.
	gs, err := shard.NewGRPCShard(shardID, addr)
	if err != nil {
		p.launcher.TerminateInstance(ctx, addr)
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}

	// Configure RPC with retry — instance may need a moment to start.
	cfg := &pb.ConfigureRequest{
		ShardId:     shardID,
		Apps:        apps,
		WindowStart: windowStart.Format(time.RFC3339),
		WindowEnd:   windowEnd.Format(time.RFC3339),
	}
	var configErr error
	for i := 0; i < 12; i++ {
		configErr = gs.Configure(ctx, cfg)
		if configErr == nil {
			break
		}
		log.Printf("provisioner: configure %s attempt %d: %v", shardID, i+1, configErr)
		time.Sleep(5 * time.Second)
	}
	if configErr != nil {
		p.launcher.TerminateInstance(ctx, addr)
		return nil, fmt.Errorf("configure %s: %w", shardID, configErr)
	}

	log.Printf("provisioner: shard %s ready at %s", shardID, addr)

	return &ShardEntry{
		ID:          shardID,
		Apps:        apps,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		State:       StateIngesting,
		Addr:        addr,
		Shard:       gs,
		CreatedAt:   time.Now(),
	}, nil
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

	// Archive READ_ONLY shards past retention period.
	if p.config.RetentionPeriod > 0 && p.config.ParquetDir != "" {
		for _, entry := range p.registry.Entries() {
			if entry.State != StateReadOnly {
				continue
			}
			if time.Since(entry.CreatedAt) > p.config.RetentionPeriod {
				log.Printf("provisioner: archiving shard %s", entry.ID)
				p.archive(entry)
			}
		}
	}
}

// archive exports a READ_ONLY shard to Parquet and replaces it with a ColdShard.
func (p *Provisioner) archive(entry *ShardEntry) {
	appLabel := joinApps(entry.Apps)
	parquetPath := fmt.Sprintf("%s/%s/%s.parquet",
		p.config.ParquetDir, appLabel, entry.ID)

	s3cfg := p.config.S3Config
	isS3 := strings.HasPrefix(p.config.ParquetDir, "s3://")

	ds, ok := entry.Shard.(*shard.DuckDBShard)
	if !ok {
		log.Printf("provisioner: cannot export non-DuckDB shard %s (type %T)", entry.ID, entry.Shard)
		return
	}

	if isS3 && s3cfg != nil {
		// Export directly to S3 via DuckDB httpfs.
		if err := ds.ExportToS3Parquet(parquetPath, s3cfg); err != nil {
			log.Printf("provisioner: S3 export %s failed: %v", entry.ID, err)
			return
		}
	} else {
		// Export to local filesystem.
		if err := os.MkdirAll(filepath.Dir(parquetPath), 0755); err != nil {
			log.Printf("provisioner: mkdir for %s: %v", parquetPath, err)
			return
		}
		if err := ds.ExportToParquet(parquetPath); err != nil {
			log.Printf("provisioner: export %s failed: %v", entry.ID, err)
			return
		}
	}

	// Create cold shard — Lambda if function URL is configured, in-process otherwise.
	var coldShard shard.Shard
	if p.config.LambdaFunctionURL != "" {
		coldShard = shard.NewLambdaColdShard(entry.ID, entry.Apps, p.config.LambdaFunctionURL, parquetPath)
	} else {
		cs, err := shard.NewColdShard(entry.ID, entry.Apps, parquetPath, s3cfg)
		if err != nil {
			log.Printf("provisioner: create cold shard for %s: %v", entry.ID, err)
			return
		}
		coldShard = cs
	}

	// Swap: close old shard, replace with cold.
	entry.Shard.Close()
	entry.Shard = coldShard
	entry.State = StateArchived
	entry.ParquetPath = parquetPath

	// Persist the state change.
	p.registry.PersistState(entry)

	log.Printf("provisioner: shard %s archived to %s", entry.ID, parquetPath)
}

// rotate transitions an INGESTING shard to READ_ONLY and launches a replacement.
func (p *Provisioner) rotate(ctx context.Context, old *ShardEntry) {
	now := time.Now().UTC()
	seq := p.registry.Count()
	newID := fmt.Sprintf("shard-%s-%s-%d", joinApps(old.Apps), now.Format("20060102T1504"), seq)
	farFuture := now.Add(100 * 365 * 24 * time.Hour)

	entry, err := p.launchAndConfigure(ctx, newID, "small", old.Apps, now, farFuture)
	if err != nil {
		log.Printf("provisioner: failed to launch replacement for %s: %v", old.ID, err)
		return
	}

	p.registry.Add(entry)

	// Transition old shard.
	old.WindowEnd = now
	old.State = StateReadOnly
	log.Printf("provisioner: rotated %s → READ_ONLY, new shard %s", old.ID, newID)
}

// provisionForBucket launches a new INGESTING shard for a bucket.
func (p *Provisioner) provisionForBucket(ctx context.Context, bucket *AppBucket) {
	if _, isASG := p.launcher.(*ASGLauncher); isASG {
		log.Printf("provisioner: launching shard for bucket %s via ASG", bucket.ID)
		if _, err := p.launcher.LaunchInstance(ctx, "small"); err != nil {
			log.Printf("provisioner: failed to launch shard for bucket %s: %v", bucket.ID, err)
		}
		return
	}

	now := time.Now().UTC()
	seq := p.registry.Count()
	id := fmt.Sprintf("shard-%s-%s-%d", joinApps(bucket.Apps), now.Format("20060102T1504"), seq)
	farFuture := now.Add(100 * 365 * 24 * time.Hour)

	entry, err := p.launchAndConfigure(ctx, id, "small", bucket.Apps, now, farFuture)
	if err != nil {
		log.Printf("provisioner: failed to launch shard for bucket %s: %v", bucket.ID, err)
		return
	}

	p.registry.Add(entry)
	log.Printf("provisioner: shard %s launched for bucket %s", id, bucket.ID)
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
