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

// reapGracePeriod is how long a newly-added registry entry is
// protected from reap, measured from its CreatedAt. Must cover one
// provisioner tick plus a heartbeat round-trip — otherwise a shard
// that got christened this tick but hasn't yet heartbeated with its
// minted id gets killed on the same tick. 2 min matches the
// LiveShardHeartbeats staleness window.
const reapGracePeriod = 2 * time.Minute

// HeartbeatStore is the interface the provisioner needs for heartbeat operations.
type HeartbeatStore interface {
	LiveShardHeartbeats(since string) ([]store.ShardHeartbeat, error)
	InsertHeartbeat(hb *store.ShardHeartbeat) error
}

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
	Store              HeartbeatStore  // for reading shard heartbeats (nil = no heartbeat adoption)
	WindowDuration     time.Duration   // duration of a single shard window (w.duration in docs/shard-lifecycle.md)
	ConfigureRetries   int             // attempts to Configure a freshly-launched shard (default 12)
	ConfigureBackoff   time.Duration   // sleep between Configure attempts (default 5s)
	// ShardSecret is the shared bearer token attached to every outbound
	// coord→shard gRPC call (see auth.ClientTokenUnaryInterceptor). The
	// shard's server-side interceptor validates against the same value.
	// Empty is only valid in in-process tests where the shard server
	// runs without an interceptor — production deploys populate this
	// from SHARD_SECRET via cmd/coordinator/main.go.
	ShardSecret string
}

// DefaultProvisionerConfig returns sensible production defaults.
func DefaultProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		MaxRows:            5_000_000,
		MaxDiskBytes:       500 * 1024 * 1024, // 500MB
		MaxDuration:        8 * time.Hour,
		PollInterval:       5 * time.Minute,
		IngestingPerBucket: 2,
		WindowDuration:     7 * 24 * time.Hour,
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
		WindowDuration:     1 * time.Hour,
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

// ShardConfigurer dials a shard at addr, sends Configure, and returns a
// live shard handle. Abstracted so tests can avoid real gRPC.
type ShardConfigurer interface {
	Dial(ctx context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error)
}

// grpcConfigurer is the production ShardConfigurer — it actually dials
// the shard over gRPC. Holds the shared secret so every dial attaches
// the SHARD_SECRET bearer for the shard-side interceptor.
type grpcConfigurer struct {
	shardSecret string
}

func (c grpcConfigurer) Dial(ctx context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error) {
	gs, err := shard.NewGRPCShard(req.ShardId, addr, c.shardSecret)
	if err != nil {
		return nil, err
	}
	if err := gs.Configure(ctx, req); err != nil {
		gs.Close()
		return nil, err
	}
	return gs, nil
}

// Provisioner monitors shard fill rates and handles rotation.
type Provisioner struct {
	registry    *ShardRegistry
	launcher    ShardLauncher
	config      ProvisionerConfig
	stats       *AppStats
	Buckets     *BucketManager
	configurer  ShardConfigurer
	volumes     VolumeManager // optional; used to tag data volumes at christen time
	heartbeatMu sync.Mutex    // serialises ProcessHeartbeat with the ticker's adoptFromHeartbeats
}

// NewProvisioner creates a provisioner.
func NewProvisioner(registry *ShardRegistry, launcher ShardLauncher, config ProvisionerConfig) *Provisioner {
	return &Provisioner{
		registry:   registry,
		launcher:   launcher,
		config:     config,
		stats:      NewAppStats(),
		Buckets:    NewBucketManager(nil),
		configurer: grpcConfigurer{shardSecret: config.ShardSecret},
	}
}

// SetStore provides the store for heartbeat queries. Called after the store
// is opened (which may happen after the provisioner starts).
func (p *Provisioner) SetStore(s *store.Store) {
	p.config.Store = s
}

// SetVolumeManager enables volume-aware christening. When set, the
// provisioner stamps the minted shard id onto the pre-attached EBS
// volume as the final step of christen().
func (p *Provisioner) SetVolumeManager(vm VolumeManager) {
	p.volumes = vm
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
// and dispatches each to ProcessHeartbeat. Runs on the provisioner
// ticker as a catch-up / reconciliation pass; the Heartbeat RPC path
// drives ProcessHeartbeat directly for sub-second latency.
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
		p.ProcessHeartbeat(hb)
	}
}

// ProcessHeartbeat handles one heartbeat synchronously:
//   - empty ShardID        → christen (mint id, Configure, register)
//   - known id at same addr → unsuspend if needed, no-op otherwise
//   - known id at new addr  → rebind (reconnect gRPC, update entry)
//   - known id not in registry → adopt (coordinator restart catchup)
//
// Exported so the Heartbeat RPC handler can drive state transitions
// immediately rather than wait for the provisioner's tick interval.
// Serialised against itself via heartbeatMu so concurrent heartbeats
// for the same unclaimed instance can't race into a double christen.
func (p *Provisioner) ProcessHeartbeat(hb store.ShardHeartbeat) {
	p.heartbeatMu.Lock()
	defer p.heartbeatMu.Unlock()

	if hb.ShardID == "" {
		if err := p.christen(hb); err != nil {
			log.Printf("provisioner: christen %s: %v", hb.Addr, err)
		}
		return
	}

	existing := p.registry.Get(hb.ShardID)
	if existing != nil {
		if existing.Addr == hb.Addr {
			if existing.Suspended {
				log.Printf("provisioner: unsuspending shard %s (heartbeat received)", hb.ShardID)
				existing.Suspended = false
			}
			return
		}
		// Rebind: same durable id, instance moved.
		gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr, p.config.ShardSecret)
		if err != nil {
			log.Printf("provisioner: reconnect %s at %s failed: %v", hb.ShardID, hb.Addr, err)
			return
		}
		log.Printf("provisioner: shard %s moved %s → %s", hb.ShardID, existing.Addr, hb.Addr)
		existing.Shard.Close()
		existing.Shard = gs
		existing.Addr = hb.Addr
		existing.InstanceID = hb.InstanceID
		existing.Suspended = false
		p.registry.PersistState(existing)
		return
	}

	// Known id but not in the registry: coordinator restart / reload
	// catchup. Adopt the entry so queries reach it.
	gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr, p.config.ShardSecret)
	if err != nil {
		log.Printf("provisioner: adopt %s at %s failed: %v", hb.ShardID, hb.Addr, err)
		return
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

// christen assigns identity to an unclaimed shard: mints a shard id,
// picks a window from WindowDuration, and sends Configure. The shard
// persists the identity to shard.json before returning success.
//
// For now we use catch-all apps (empty list) — gap item #9 in
// docs/shard-lifecycle.md tracks populating from the onboarded set.
func (p *Provisioner) christen(hb store.ShardHeartbeat) error {
	windowDur := p.config.WindowDuration
	if windowDur == 0 {
		windowDur = 7 * 24 * time.Hour
	}
	now := time.Now().UTC()
	windowStart := now.Truncate(windowDur)
	windowEnd := windowStart.Add(windowDur)

	// Name format: shard-YYYYMMDDThhmm-seq. Globally unique within the
	// fleet; seq increments from current registry size so concurrent
	// christenings don't collide.
	seq := p.registry.Count() + 1
	shardID := fmt.Sprintf("shard-%s-%d", windowStart.Format("20060102T1504"), seq)

	// Mint a per-shard bearer for heartbeat auth. The shard persists
	// this in shard.json (see Configure) and sends it as authorization
	// metadata on every subsequent Heartbeat RPC. Without this
	// secret an attacker with network reach to :9020 could register a
	// rogue shard by heartbeating with a stolen ID.
	heartbeatToken, err := store.GenerateShardHeartbeatToken()
	if err != nil {
		return fmt.Errorf("mint heartbeat token for %s: %w", shardID, err)
	}

	req := &pb.ConfigureRequest{
		ShardId:        shardID,
		Apps:           nil, // catch-all; see gap #9
		WindowStart:    windowStart.Format(time.RFC3339),
		WindowEnd:      windowEnd.Format(time.RFC3339),
		HeartbeatToken: heartbeatToken,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sh, err := p.configurer.Dial(ctx, hb.Addr, req)
	if err != nil {
		return fmt.Errorf("configure %s at %s: %w", shardID, hb.Addr, err)
	}

	// Find the data volume the lifecycle handler attached for this
	// instance (tagged shard_id=pending-<instance_id>) and rewrite
	// the shard_id tag to the now-known real id. Caches the volume
	// id on the entry so recovery can skip DescribeVolumes.
	var volumeID string
	if p.volumes != nil && hb.InstanceID != "" {
		placeholder := "pending-" + hb.InstanceID
		if info, err := p.volumes.FindByShard(ctx, placeholder); err != nil {
			log.Printf("provisioner: find volume for %s: %v", placeholder, err)
		} else if info != nil {
			if err := p.volumes.TagWithShardID(ctx, info.VolumeID, shardID); err != nil {
				log.Printf("provisioner: tag volume %s as %s: %v", info.VolumeID, shardID, err)
			} else {
				volumeID = info.VolumeID
			}
		}
	}

	p.registry.Add(&ShardEntry{
		ID:             shardID,
		Apps:           nil,
		WindowStart:    windowStart,
		WindowEnd:      windowEnd,
		State:          StateIngesting,
		Addr:           hb.Addr,
		InstanceID:     hb.InstanceID,
		RowCount:       hb.RowCount,
		EBSVolumeID:    volumeID,
		Shard:          sh,
		CreatedAt:      now,
		HeartbeatToken: heartbeatToken,
	})
	log.Printf("provisioner: christened %s at %s (window %s–%s, volume=%s)",
		shardID, hb.Addr, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), volumeID)
	return nil
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

		// Grace period for freshly-christened shards. adoptFromHeartbeats
		// and reapDeadShards run in the same tick; a shard christened
		// during adopt has a brand-new registry id but the heartbeat
		// that triggered its christening still carried the old
		// empty-id ping. Without this guard, reap sees the new entry
		// absent from `live` and kills it on the same tick. The real
		// heartbeat (carrying the minted id) lands a few seconds
		// later.
		if time.Since(entry.CreatedAt) < reapGracePeriod {
			continue
		}

		// Failure-recovery branch: if the shard has a bound EBS
		// volume, its data outlives the instance. The ASG's
		// health-check replaces the instance; the lifecycle hook
		// reattaches the volume to the replacement; the replacement
		// heartbeats with the same shard id and the rebind arm of
		// adoptFromHeartbeats rewires the registry entry. We must
		// preserve the registry entry through the stale-heartbeat
		// window so rebind has something to update.
		if entry.EBSVolumeID != "" {
			log.Printf("provisioner: shard %s stale heartbeat — preserving for ASG-driven recovery (vol=%s)",
				entry.ID, entry.EBSVolumeID)
			continue
		}

		// Legacy path: no EBS volume → data is already lost with the
		// instance. Remove from registry and ask the launcher to
		// terminate so the ASG doesn't keep a ghost instance.
		log.Printf("provisioner: reaping dead shard %s (no heartbeat, no volume)", entry.ID)
		p.registry.Remove(entry.ID)

		if p.launcher != nil && (entry.InstanceID != "" || entry.Addr != "") {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := p.launcher.TerminateInstance(ctx, entry.InstanceID, entry.Addr); err != nil {
				log.Printf("provisioner: terminate %s (%s): %v", entry.ID, entry.Addr, err)
			}
			cancel()
		}
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
// Goes through the ShardConfigurer so tests can swap the gRPC dial for
// a stub.
func (p *Provisioner) launchAndConfigure(ctx context.Context, shardID, tier string, apps []string, windowStart, windowEnd time.Time) (*ShardEntry, error) {
	log.Printf("provisioner: launching shard %s (tier=%s)", shardID, tier)

	addr, err := p.launcher.LaunchInstance(ctx, tier)
	if err != nil {
		return nil, fmt.Errorf("launch: %w", err)
	}

	cfg := &pb.ConfigureRequest{
		ShardId:     shardID,
		Apps:        apps,
		WindowStart: windowStart.Format(time.RFC3339),
		WindowEnd:   windowEnd.Format(time.RFC3339),
	}

	// Retry Configure — instance may still be coming up. The
	// configurer is responsible for the actual gRPC dial.
	retries := p.config.ConfigureRetries
	if retries <= 0 {
		retries = 12
	}
	backoff := p.config.ConfigureBackoff
	if backoff <= 0 {
		backoff = 5 * time.Second
	}

	var sh shard.Shard
	var configErr error
	for i := 0; i < retries; i++ {
		sh, configErr = p.configurer.Dial(ctx, addr, cfg)
		if configErr == nil {
			break
		}
		log.Printf("provisioner: configure %s attempt %d: %v", shardID, i+1, configErr)
		time.Sleep(backoff)
	}
	if configErr != nil {
		p.launcher.TerminateInstance(ctx, "", addr)
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
		Shard:       sh,
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

	now := time.Now().UTC()

	for _, entry := range p.registry.Entries() {
		if entry.State != StateIngesting {
			continue
		}

		// Wall-clock window boundary: once we've passed WindowEnd,
		// this shard should serve queries only. Transition and
		// persist; a replacement will be christened by the next
		// heartbeat from an ASG-launched instance.
		if !entry.WindowEnd.IsZero() && !now.Before(entry.WindowEnd) {
			log.Printf("provisioner: rotating shard %s → READ_ONLY (window ended %v ago)",
				entry.ID, now.Sub(entry.WindowEnd).Round(time.Second))
			entry.State = StateReadOnly
			p.registry.PersistState(entry)
			continue
		}

		// Legacy row-count / MaxDuration rotation, retained for
		// dev/docker where windows aren't set. Triggers a full
		// rotate() including launchAndConfigure for a replacement.
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

	// Archive READ_ONLY shards once their window-end is more than
	// RetentionPeriod in the past. Measuring from WindowEnd (not
	// CreatedAt) keeps a long-lived shard hot for a full retention
	// period after it transitions — CreatedAt is the christening
	// time, not when the shard stopped taking writes.
	if p.config.RetentionPeriod > 0 && p.config.ParquetDir != "" {
		for _, entry := range p.registry.Entries() {
			if entry.State != StateReadOnly {
				continue
			}
			if entry.WindowEnd.IsZero() {
				continue // legacy/zero-window shards are excluded
			}
			if now.Sub(entry.WindowEnd) <= p.config.RetentionPeriod {
				continue
			}
			log.Printf("provisioner: archiving shard %s (%v past WindowEnd)",
				entry.ID, now.Sub(entry.WindowEnd).Round(time.Second))
			p.archive(entry)
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

	// Retire the data volume: snapshot → detach → delete. Skip when
	// no volume is bound (shards from the pre-S5 era, or dev-mode
	// shards). Snapshot first so a detach/delete failure after a
	// successful snapshot leaves us with durable data; a snapshot
	// failure aborts the whole sequence and leaves the volume intact
	// for a retry.
	if p.volumes != nil && entry.EBSVolumeID != "" {
		p.retireVolume(entry)
	}
}

// retireVolume runs the snapshot → detach → delete sequence on an
// archived shard's data volume. Errors are logged rather than
// returned; the archive state change has already been persisted and
// we don't want to undo it on a terminal-side failure that an
// operator can clean up out of band.
func (p *Provisioner) retireVolume(entry *ShardEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	snapID, err := p.volumes.SnapshotVolume(ctx, SnapshotOpts{
		VolumeID:    entry.EBSVolumeID,
		ShardID:     entry.ID,
		ParquetPath: entry.ParquetPath,
	})
	if err != nil {
		log.Printf("provisioner: snapshot %s (%s): %v — leaving volume in place for retry",
			entry.EBSVolumeID, entry.ID, err)
		return
	}
	log.Printf("provisioner: snapshotted %s → %s before archive delete", entry.EBSVolumeID, snapID)

	// Detach with force=true: the instance hosting this shard may
	// already have been terminated via the ASG path, in which case a
	// non-force detach hangs. Even on a live instance the volume is
	// now unused.
	if err := p.volumes.DetachFromInstance(ctx, entry.EBSVolumeID, true); err != nil {
		log.Printf("provisioner: detach %s: %v", entry.EBSVolumeID, err)
		return
	}
	if err := p.volumes.DeleteVolume(ctx, entry.EBSVolumeID); err != nil {
		log.Printf("provisioner: delete %s: %v", entry.EBSVolumeID, err)
		return
	}
	log.Printf("provisioner: retired volume %s (shard %s)", entry.EBSVolumeID, entry.ID)

	entry.EBSVolumeID = ""
	p.registry.PersistState(entry)
}

// rotate transitions an INGESTING shard to READ_ONLY and launches a
// replacement. The transition on `old` is persisted unconditionally
// — even when the replacement launch fails — so the store doesn't
// disagree with the registry about what state `old` is in.
func (p *Provisioner) rotate(ctx context.Context, old *ShardEntry) {
	now := time.Now().UTC()
	seq := p.registry.Count()
	newID := fmt.Sprintf("shard-%s-%s-%d", joinApps(old.Apps), now.Format("20060102T1504"), seq)
	farFuture := now.Add(100 * 365 * 24 * time.Hour)

	// Transition first, then launch. If launch fails we've at least
	// stopped routing writes to `old`; the next check() will attempt
	// the replacement again, and the write path falls back to the
	// remaining INGESTING shards in the bucket.
	old.WindowEnd = now
	old.State = StateReadOnly
	p.registry.PersistState(old)

	entry, err := p.launchAndConfigure(ctx, newID, "small", old.Apps, now, farFuture)
	if err != nil {
		log.Printf("provisioner: failed to launch replacement for %s: %v", old.ID, err)
		return
	}

	p.registry.Add(entry)
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
