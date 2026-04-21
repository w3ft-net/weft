package coordinator

import (
	"context"
	"encoding/json"
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
	PollInterval       time.Duration   // how often to check (default 5min)
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
// IngestingPerBucket isn't here — that target lives on each bucket
// row in the DB (see migration 020).
func DefaultProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		PollInterval:    5 * time.Minute,
		WindowDuration:  7 * 24 * time.Hour,
		RetentionPeriod: 7 * 24 * time.Hour,
	}
}

// DevProvisionerConfig returns faster thresholds for development/testing.
func DevProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		PollInterval:   5 * time.Second,
		WindowDuration: 1 * time.Hour,
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
	Buckets     Buckets
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
	// catchup. Adopt the entry so queries reach it — but first pull
	// the persisted record from the store so fields the heartbeat
	// doesn't carry (window, heartbeat token, apps, created-at)
	// survive the round-trip. Without this, registry.Add →
	// SaveShard (an UPSERT) would write zero-valued window + empty
	// token back over the correct row.
	gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr, p.config.ShardSecret)
	if err != nil {
		log.Printf("provisioner: adopt %s at %s failed: %v", hb.ShardID, hb.Addr, err)
		return
	}

	state := StateIngesting
	if hb.State == "READ_ONLY" {
		state = StateReadOnly
	}

	entry := &ShardEntry{
		ID:         hb.ShardID,
		Addr:       hb.Addr,
		InstanceID: hb.InstanceID,
		State:      state,
		RowCount:   hb.RowCount,
		Shard:      gs,
		CreatedAt:  time.Now(),
	}

	if p.registry.store != nil {
		if rec, ok, err := p.registry.store.GetShard(hb.ShardID); err != nil {
			log.Printf("provisioner: adopt %s — store lookup failed, falling back to defaults: %v", hb.ShardID, err)
		} else if ok {
			if ws, err := time.Parse(time.RFC3339, rec.WindowStart); err == nil {
				entry.WindowStart = ws
			}
			if we, err := time.Parse(time.RFC3339, rec.WindowEnd); err == nil {
				entry.WindowEnd = we
			}
			if ca, err := time.Parse(time.RFC3339, rec.CreatedAt); err == nil {
				entry.CreatedAt = ca
			}
			entry.HeartbeatToken = rec.HeartbeatToken
			entry.Tier = rec.Tier
			entry.EBSVolumeID = rec.EBSVolumeID
			entry.BucketID = rec.BucketID
			if rec.Apps != "" && rec.Apps != "null" {
				_ = json.Unmarshal([]byte(rec.Apps), &entry.Apps)
			}
			// Preserve READ_ONLY from DB over an INGESTING heartbeat —
			// the DB is the authoritative state for lifecycle.
			if ShardState(rec.State) == StateReadOnly {
				entry.State = StateReadOnly
			}
		} else {
			log.Printf("provisioner: adopt %s has no store record — registering with defaults (anomalous)",
				hb.ShardID)
		}
	}

	p.registry.Add(entry)
	log.Printf("provisioner: adopted shard %s at %s (state=%s, rows=%d)",
		hb.ShardID, hb.Addr, hb.State, hb.RowCount)
}

// christen assigns identity to an unclaimed shard: looks up the
// bucket the shard should belong to, mints a shard id, picks a window
// from WindowDuration, and sends Configure. The shard persists the
// identity to shard.json before returning success.
//
// Today every christened shard goes into the catch-all "default"
// bucket (BucketManager.Default). When per-app dedicated buckets are
// added (the Split path), this lookup grows logic to pick by app /
// instance metadata.
func (p *Provisioner) christen(hb store.ShardHeartbeat) error {
	// Race guard: two heartbeats from the same unclaimed instance
	// arrive back-to-back (the Heartbeat RPC dispatches each in its
	// own goroutine). heartbeatMu serialises them, but the second
	// goroutine's hb.ShardID is still "" because it was captured at
	// send time, before the first Configure returned. Without this
	// check the second goroutine mints a new seq and tries to
	// re-Configure the same addr; the shard then rejects with
	// "shard already configured as ...".
	if p.registry.HasAddr(hb.Addr) {
		return nil
	}

	bucket, err := p.Buckets.Default()
	if err != nil {
		return fmt.Errorf("christen %s: lookup default bucket: %w", hb.Addr, err)
	}
	if bucket == nil {
		return fmt.Errorf("christen %s: no default bucket — has migration 020+022 run?", hb.Addr)
	}

	now := time.Now().UTC()
	windowStart, windowEnd := p.currentWindow(bucket, now)

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

	// Snapshot the bucket's current Apps into the shard. After this
	// point the shard's app set is frozen — adding an app to the
	// bucket later does NOT backfill it onto already-christened
	// shards (intentional: those shards never received writes for
	// the new app, so query routing should keep them out of its
	// fan-out). Only newly-christened shards inherit the updated set.
	apps := append([]string(nil), bucket.Apps...)

	req := &pb.ConfigureRequest{
		ShardId:        shardID,
		Apps:           apps,
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
		Apps:           apps,
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
		Tier:           bucket.Tier,
		BucketID:       bucket.ID,
	})
	log.Printf("provisioner: christened %s at %s (bucket=%s(%d) window %s–%s volume=%s)",
		shardID, hb.Addr, bucket.Label, bucket.ID, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), volumeID)
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

// currentWindow returns the [start, end) window bounds enclosing now,
// aligned to the bucket's WindowDuration (falling back to the
// provisioner config, then to 7d). Per-bucket durations (migration
// 021) let different buckets run at different cadences on the same
// tier.
func (p *Provisioner) currentWindow(bucket *AppBucket, now time.Time) (time.Time, time.Time) {
	dur := p.windowDurationFor(bucket)
	start := now.UTC().Truncate(dur)
	return start, start.Add(dur)
}

// windowDurationFor picks the effective WindowDuration for a bucket,
// with a two-step fallback so tests and boot-before-hydration paths
// stay working. Production paths always pass a bucket with a set
// WindowDuration (persisted by migration 021).
func (p *Provisioner) windowDurationFor(bucket *AppBucket) time.Duration {
	if bucket != nil && bucket.WindowDuration > 0 {
		return bucket.WindowDuration
	}
	if p.config.WindowDuration > 0 {
		return p.config.WindowDuration
	}
	return 7 * 24 * time.Hour
}

// retentionPeriodFor picks the effective RetentionPeriod for a
// bucket, same fallback chain as windowDurationFor. Zero means
// "don't archive this bucket" — matches the provisioner-config
// convention.
func (p *Provisioner) retentionPeriodFor(bucket *AppBucket) time.Duration {
	if bucket != nil && bucket.RetentionPeriod > 0 {
		return bucket.RetentionPeriod
	}
	return p.config.RetentionPeriod
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

	// Local launcher: configure immediately. Align the bootstrap
	// shard's window to the bucket's WindowDuration boundary so
	// check()'s wall-clock rotation fires on the same schedule as
	// christened shards. Bootstrap needs the default bucket — bail
	// loudly if it's missing rather than silently degrading to a
	// shard with no bucket_id (which would fail the NOT NULL FK at
	// persist time anyway).
	if p.Buckets == nil {
		log.Printf("provisioner: bootstrap failed: no BucketManager configured")
		return
	}
	bucket, err := p.Buckets.Default()
	if err != nil {
		log.Printf("provisioner: bootstrap failed: lookup default bucket: %v", err)
		return
	}
	if bucket == nil {
		log.Printf("provisioner: bootstrap failed: no default bucket")
		return
	}

	tier := bucket.Tier
	if tier == "" {
		tier = "small"
	}
	apps := append([]string(nil), bucket.Apps...)

	now := time.Now().UTC()
	windowStart, windowEnd := p.currentWindow(bucket, now)

	shardID := fmt.Sprintf("shard-%d", now.UnixMilli())
	entry, err := p.launchAndConfigure(ctx, shardID, tier, apps, windowStart, windowEnd)
	if err != nil {
		log.Printf("provisioner: bootstrap failed: %v", err)
		return
	}
	entry.BucketID = bucket.ID
	entry.Tier = tier

	p.registry.Add(entry)
	log.Printf("provisioner: bootstrapped shard %s (bucket=%s(%d) %s to %s)",
		shardID, bucket.Label, bucket.ID, now.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
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

// OnboardApp records an app as belonging to a bucket and fans the
// new app out to every INGESTING shard currently in that bucket.
// Called from AddSource (operator config) and from IngestStream on
// first-seen apps (auto-discovery). For now every app lands in the
// default catch-all bucket; a future Split() can promote individual
// apps into their own buckets.
//
// Idempotent: if the app is already on the bucket, no DB write and
// no shard mutation. Safe to call on the hot ingest path where
// SeenApps.Add returns true for every row the coordinator has ever
// seen from a given (customer, app) pair.
func (p *Provisioner) OnboardApp(app string) error {
	if p.Buckets == nil {
		return nil
	}
	bucket, err := p.Buckets.Default()
	if err != nil {
		return fmt.Errorf("OnboardApp(%s): lookup default: %w", app, err)
	}
	if bucket == nil {
		log.Printf("provisioner: OnboardApp(%s): no default bucket — skipping", app)
		return nil
	}
	updated, err := p.Buckets.AddAppToBucket(bucket.ID, app)
	if err != nil {
		return fmt.Errorf("add %q to bucket %d: %w", app, bucket.ID, err)
	}
	if updated == nil {
		return nil
	}
	p.propagateAppsToIngesting(updated)
	return nil
}

// propagateAppsToIngesting appends any bucket apps missing from an
// INGESTING shard's Apps list to that shard, in memory and in the
// store. Only INGESTING shards mutate — READ_ONLY / ARCHIVED shards'
// apps stay frozen because they never accepted writes for the new
// app, and including them in query routing would widen the fan-out
// pointlessly.
func (p *Provisioner) propagateAppsToIngesting(bucket *AppBucket) {
	for _, entry := range p.registry.Entries() {
		if entry.State != StateIngesting {
			continue
		}
		if entry.BucketID != bucket.ID {
			continue
		}
		changed := false
		have := make(map[string]bool, len(entry.Apps))
		for _, a := range entry.Apps {
			have[a] = true
		}
		for _, a := range bucket.Apps {
			if !have[a] {
				entry.Apps = append(entry.Apps, a)
				changed = true
			}
		}
		if changed {
			p.registry.PersistState(entry)
		}
	}
}

// check evaluates each INGESTING shard for rotation and app split.
func (p *Provisioner) check(ctx context.Context) {
	// Evaluate bucket splits based on ingestion stats.
	if p.Buckets != nil {
		snapshot := p.stats.Snapshot()
		app, err := p.Buckets.EvaluateSplit(snapshot)
		if err != nil {
			log.Printf("provisioner: EvaluateSplit: %v", err)
		} else if app != "" {
			bucket, err := p.Buckets.Split(app)
			if err != nil {
				log.Printf("provisioner: Split(%s): %v", app, err)
			} else if bucket != nil {
				log.Printf("provisioner: splitting %s into dedicated shard", app)
				p.provisionForBucket(ctx, bucket)
			}
		}
	}

	// Ensure each bucket has enough INGESTING shards. The target
	// comes from the bucket's persisted record (DB-backed since
	// migration 020) rather than a per-coordinator config knob.
	if p.Buckets != nil {
		all, err := p.Buckets.All()
		if err != nil {
			log.Printf("provisioner: Buckets.All: %v", err)
			all = nil
		}
		for _, bucket := range all {
			if bucket.Target <= 0 {
				continue
			}
			// For multi-app buckets, count INGESTING shards that own
			// the first app — they all share the same set so the
			// count is identical for any app in the bucket. For the
			// catch-all default bucket (no apps), pass "" which
			// hasApp() treats as a wildcard match against catch-all
			// shards.
			var probeApp string
			if len(bucket.Apps) > 0 {
				probeApp = bucket.Apps[0]
			}
			current := p.registry.IngestingCount(probeApp)
			for current < bucket.Target {
				log.Printf("provisioner: scaling up bucket %d (%d/%d ingesting shards)",
					bucket.ID, current, bucket.Target)
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
		// heartbeat from an ASG-launched instance (or by the bucket-
		// sizing loop above for local/dev launchers).
		if !entry.WindowEnd.IsZero() && !now.Before(entry.WindowEnd) {
			log.Printf("provisioner: rotating shard %s → READ_ONLY (window ended %v ago)",
				entry.ID, now.Sub(entry.WindowEnd).Round(time.Second))
			entry.State = StateReadOnly
			p.registry.PersistState(entry)
			continue
		}
	}

	// Archive READ_ONLY shards once their window-end is more than
	// RetentionPeriod in the past. Measuring from WindowEnd (not
	// CreatedAt) keeps a long-lived shard hot for a full retention
	// period after it transitions — CreatedAt is the christening
	// time, not when the shard stopped taking writes. Retention is
	// per-bucket (migration 021); we resolve it by looking up the
	// entry's bucket, then fall back to config for legacy/bare
	// entries that haven't been rebound to a bucket yet.
	if p.config.ParquetDir != "" {
		bucketByID := make(map[int64]*AppBucket)
		if p.Buckets != nil {
			all, err := p.Buckets.All()
			if err != nil {
				log.Printf("provisioner: archive lookup Buckets.All: %v", err)
			}
			for _, b := range all {
				bucketByID[b.ID] = b
			}
		}
		for _, entry := range p.registry.Entries() {
			if entry.State != StateReadOnly {
				continue
			}
			if entry.WindowEnd.IsZero() {
				continue // legacy/zero-window shards are excluded
			}
			retention := p.retentionPeriodFor(bucketByID[entry.BucketID])
			if retention <= 0 {
				continue // archive disabled for this bucket
			}
			if now.Sub(entry.WindowEnd) <= retention {
				continue
			}
			log.Printf("provisioner: archiving shard %s (%v past WindowEnd, retention=%v)",
				entry.ID, now.Sub(entry.WindowEnd).Round(time.Second), retention)
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

// provisionForBucket launches a new INGESTING shard for a bucket.
// Tier comes from the bucket record so per-bucket fleet sizing (small
// catch-all vs. medium dedicated, etc.) is data-driven.
func (p *Provisioner) provisionForBucket(ctx context.Context, bucket *AppBucket) {
	tier := bucket.Tier
	if tier == "" {
		tier = "small"
	}

	if _, isASG := p.launcher.(*ASGLauncher); isASG {
		log.Printf("provisioner: launching shard for bucket %d via ASG (tier=%s)", bucket.ID, tier)
		if _, err := p.launcher.LaunchInstance(ctx, tier); err != nil {
			log.Printf("provisioner: failed to launch shard for bucket %d: %v", bucket.ID, err)
		}
		return
	}

	// Local-launcher path: mint a window aligned to the bucket's
	// WindowDuration boundary. Matches christen() so bucket-
	// provisioned shards and heartbeat-christened shards share
	// window semantics.
	now := time.Now().UTC()
	windowStart, windowEnd := p.currentWindow(bucket, now)
	seq := p.registry.Count()
	id := fmt.Sprintf("shard-%s-%s-%d", joinApps(bucket.Apps), windowStart.Format("20060102T1504"), seq)

	entry, err := p.launchAndConfigure(ctx, id, tier, bucket.Apps, windowStart, windowEnd)
	if err != nil {
		log.Printf("provisioner: failed to launch shard for bucket %d: %v", bucket.ID, err)
		return
	}
	entry.BucketID = bucket.ID
	entry.Tier = tier

	p.registry.Add(entry)
	log.Printf("provisioner: shard %s launched for bucket %d", id, bucket.ID)
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
