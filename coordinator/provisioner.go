package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

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

// ShardOpsStore is the shard_records operations the provisioner and
// lifecycle hook need for the LAUNCHING/INGESTING handoff. The
// reconciler INSERTs LAUNCHING rows, the lifecycle hook claims them,
// the christen path promotes them, the reaper cleans up stale ones.
// Separate from HeartbeatStore so tests that don't exercise this path
// can keep their minimal mocks.
//
// *store.Store satisfies this interface natively; tests that do
// exercise the LAUNCHING lifecycle supply a fake.
type ShardOpsStore interface {
	SaveShard(rec *store.ShardRecord) error
	DeleteShard(id string) error
	ClaimLaunchingForTier(tier, instanceID string) (store.ShardRecord, bool, error)
	FindLaunchingByInstance(instanceID string) (store.ShardRecord, bool, error)
	ListStaleLaunching(olderThan time.Duration) ([]store.ShardRecord, error)
	CountShardsInBucket(bucketID int64, states []string) (int, error)
	CountShardsInBucketForFutureWindows(bucketID int64, currentWindowStart time.Time) (int, error)
}

// staleLaunchingAge is the age beyond which a LAUNCHING row is
// considered stuck. Rows older than this with no instance_id mean the
// ASG never brought an instance up (capacity exhausted, launch
// failed); rows with an instance_id mean the instance was claimed by
// the lifecycle hook but never heartbeated (bad AMI, userdata
// failure). 10 minutes is well past the ~2 minute instance-boot
// window plus a coordinator tick.
const staleLaunchingAge = 10 * time.Minute

// ProvisionerConfig controls shard rotation thresholds.
type ProvisionerConfig struct {
	PollInterval      time.Duration   // how often to check (default 5min)
	ParquetDir        string          // directory for Parquet exports (local path or s3:// prefix)
	S3Config          *shard.S3Config // S3 credentials for Parquet export/read (nil = local only)
	LambdaFunctionURL string          // Lambda function URL for cold queries (empty = in-process ColdShard)
	Store             HeartbeatStore  // for reading shard heartbeats (nil = no heartbeat adoption)
	WindowDuration    time.Duration   // duration of a single shard window (w.duration in docs/shard-lifecycle.md)
	ConfigureRetries  int             // attempts to Configure a freshly-launched shard (default 12)
	ConfigureBackoff  time.Duration   // sleep between Configure attempts (default 5s)
	// ShardSecret is the shared bearer token attached to every outbound
	// coord→shard gRPC call (see auth.ClientTokenUnaryInterceptor). The
	// shard's server-side interceptor validates against the same value.
	// Empty is only valid in in-process tests where the shard server
	// runs without an interceptor — production deploys populate this
	// from SHARD_SECRET via cmd/coordinator/main.go.
	ShardSecret string
	// ShardOps is the shard_records store for LAUNCHING-row lifecycle
	// (mint, claim, promote, reap). Nil in tests that don't exercise
	// the ASG handoff; main.go sets it to the full *store.Store in
	// production.
	ShardOps ShardOpsStore
	// CloseRebindGrace is how long the rebind path waits before
	// closing the old gRPC conn after a shard's address changes.
	// Long enough that in-flight queries holding a reference to the
	// old shard can complete on the old conn rather than getting cut
	// with code=Canceled mid-stream. Zero falls back to
	// defaultCloseRebindGrace.
	CloseRebindGrace time.Duration
	// AWSCredentials and AWSRegion enable SigV4 signing on Lambda
	// function URL requests when archiving shards. Populated from
	// the coordinator's aws.Config in cmd/coordinator/main.go.
	AWSCredentials aws.CredentialsProvider
	AWSRegion      string
}

// defaultCloseRebindGrace is the production default for the rebind
// close-defer window: long enough that typical user queries (<1s) and
// most cron-watch pipelines drain on the old conn, short enough that
// connection leak windows don't accumulate.
const defaultCloseRebindGrace = 5 * time.Second

// DefaultProvisionerConfig returns sensible production defaults.
// IngestingPerBucket isn't here — it lives on each bucket row in
// the DB (migration 020).
func DefaultProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		PollInterval:   5 * time.Minute,
		WindowDuration: 7 * 24 * time.Hour,
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

// SetStore provides the store for heartbeat and LAUNCHING-row
// operations. Called after the store is opened (which may happen
// after the provisioner starts). *store.Store satisfies both
// HeartbeatStore and ShardOpsStore, so one call wires both.
func (p *Provisioner) SetStore(s *store.Store) {
	p.config.Store = s
	p.config.ShardOps = s
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
				p.reapStaleLaunching()
				p.check(ctx)
			}
		}
	}()
}

// syncASGDesiredCapacity, once per tick per tier, sets the ASG's
// desired_capacity to an absolute value derived from bucket config
// and registry state — NOT from a COUNT of shard_records. For each
// tier with buckets:
//
//	desired = Σ bucket.target (buckets in this tier)
//	        + count(READ_ONLY registry entries with entry.Tier == tier)
//
// Targets cover LAUNCHING + INGESTING (a LAUNCHING in flight and an
// already-INGESTING shard both consume one target slot). READ_ONLY
// adds the count of past-window shards whose instances are still
// hot until archive.
//
// Computed desired is clamped to the ASG's MaxSize with a log
// tripwire if the clamp kicks in — "someone added a bucket to this
// tier without bumping TF's max."
//
// No-op for launchers that don't implement TierScaler (sync
// launchers); they manage their own lifecycle.
func (p *Provisioner) syncASGDesiredCapacity(ctx context.Context) {
	scaler, ok := p.launcher.(TierScaler)
	if !ok {
		return
	}
	if p.Buckets == nil {
		return
	}
	all, err := p.Buckets.All()
	if err != nil {
		log.Printf("provisioner: sync desired: Buckets.All: %v", err)
		return
	}

	// Sum targets per tier. Only tiers that have at least one bucket
	// are managed here; a tier with zero buckets has nothing the
	// coordinator wants to say about it.
	targetByTier := make(map[string]int32)
	// Aggregate next-window pre-warm LAUNCHING rows by tier. These
	// are extra capacity beyond target (target covers current-
	// window LAUNCHING+INGESTING; pre-warm rows are for the NEXT
	// window and need their own ASG slots).
	futureLaunchingByTier := make(map[string]int32)
	now := time.Now().UTC()
	for _, bucket := range all {
		if bucket.Tier == "" {
			continue
		}
		if bucket.Target > 0 {
			targetByTier[bucket.Tier] += int32(bucket.Target)
		}
		if p.config.ShardOps != nil {
			currentStart, _ := bucket.CurrentWindow(now)
			if n, err := p.config.ShardOps.CountShardsInBucketForFutureWindows(bucket.ID, currentStart); err != nil {
				log.Printf("provisioner: sync desired: bucket %d future-LAUNCHING count: %v", bucket.ID, err)
			} else if n > 0 {
				futureLaunchingByTier[bucket.Tier] += int32(n)
			}
		}
	}

	// Count READ_ONLY entries per tier from the registry. The
	// registry is the hot-state source for shards past WindowEnd
	// but still holding an instance; reaping hasn't happened yet
	// (reaping is §4 for ARCHIVED shards).
	readOnlyByTier := make(map[string]int32)
	for _, e := range p.registry.Entries() {
		if e.State != StateReadOnly {
			continue
		}
		if e.Tier == "" {
			continue
		}
		readOnlyByTier[e.Tier]++
	}

	// Union of tiers that appear anywhere — target, pre-warm, or
	// READ_ONLY. A tier with only READ_ONLY shards (no current
	// bucket) still needs its ASG sized; a pre-warm-only tier
	// would be unusual but shouldn't be lost either.
	tiers := make(map[string]struct{})
	for t := range targetByTier {
		tiers[t] = struct{}{}
	}
	for t := range futureLaunchingByTier {
		tiers[t] = struct{}{}
	}
	for t := range readOnlyByTier {
		tiers[t] = struct{}{}
	}

	for tier := range tiers {
		desired := targetByTier[tier] + readOnlyByTier[tier] + futureLaunchingByTier[tier]

		max, err := scaler.TierMaxSize(ctx, tier)
		if err != nil {
			log.Printf("provisioner: sync-desired tier=%s err=%v reason=tier-max-lookup-failed; skipping clamp",
				tier, err)
		} else if max > 0 && desired > max {
			log.Printf("provisioner: sync-desired tier=%s computed=%d max=%d clamped=true",
				tier, desired, max)
			desired = max
		}

		// One canonical line per tier per tick — every input to the
		// formula, the final desired, and the clamp ceiling. Lets the
		// on-call diff coordinator-computed desired against ASG
		// actual to localise refresh-time divergence.
		log.Printf("provisioner: sync-desired tier=%s target=%d read_only=%d future_launching=%d desired=%d max=%d",
			tier,
			targetByTier[tier],
			readOnlyByTier[tier],
			futureLaunchingByTier[tier],
			desired,
			max,
		)

		if err := scaler.SetDesiredCapacity(ctx, tier, desired); err != nil {
			log.Printf("provisioner: sync-desired tier=%s desired=%d err=%v",
				tier, desired, err)
		}
	}
}

// reapStaleLaunching sweeps LAUNCHING rows older than staleLaunchingAge.
// Two flavours, distinguished by whether the lifecycle hook has
// claimed the row:
//   - Empty instance_id: the ASG never brought an instance up
//     (capacity exhausted, AMI missing, launch template rejected).
//     DELETE the row so next tick's under-capacity op sees the gap
//     and the reconciler re-mints.
//   - Populated instance_id: the lifecycle hook claimed us but the
//     instance never heartbeated (bad AMI, userdata failure,
//     networking). Signal the ASG to terminate this specific
//     instance (decrements desired_capacity) and DELETE the row.
func (p *Provisioner) reapStaleLaunching() {
	if p.config.ShardOps == nil {
		return
	}
	stale, err := p.config.ShardOps.ListStaleLaunching(staleLaunchingAge)
	if err != nil {
		log.Printf("provisioner: list stale LAUNCHING: %v", err)
		return
	}
	for _, row := range stale {
		if row.InstanceID == "" {
			log.Printf("provisioner: reaping stale LAUNCHING %s (no instance — ASG never launched)", row.ID)
			if err := p.config.ShardOps.DeleteShard(row.ID); err != nil {
				log.Printf("provisioner: delete stale LAUNCHING %s: %v", row.ID, err)
			}
			continue
		}
		log.Printf("provisioner: abandoning stale LAUNCHING %s (instance %s never heartbeated)",
			row.ID, row.InstanceID)
		if p.launcher != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := p.launcher.TerminateInstance(ctx, row.InstanceID, ""); err != nil {
				log.Printf("provisioner: terminate stuck instance %s: %v", row.InstanceID, err)
			}
			cancel()
		}
		if err := p.config.ShardOps.DeleteShard(row.ID); err != nil {
			log.Printf("provisioner: delete stale LAUNCHING %s: %v", row.ID, err)
		}
	}
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
		existing.LastHeartbeatAt = time.Now()
		// RowCount comes from the shard's running DuckDBShard.RowCount();
		// it's the only signal the registry has about ingest-volume
		// progress on a steady-state instance. Lift it on every
		// heartbeat (same-addr OR rebind) and persist so dashboards
		// and shard_records reflect reality. Without this update the
		// row_count column froze at the value captured the moment the
		// shard was first christened/adopted.
		existing.RowCount = hb.RowCount
		if existing.Addr == hb.Addr {
			if existing.Suspended {
				log.Printf("provisioner: unsuspending shard %s (heartbeat received)", hb.ShardID)
				existing.Suspended = false
			}
			p.registry.PersistState(existing)
			return
		}
		// Rebind: same durable id, instance moved.
		gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr, p.config.ShardSecret)
		if err != nil {
			log.Printf("provisioner: reconnect %s at %s failed: %v", hb.ShardID, hb.Addr, err)
			return
		}
		log.Printf("provisioner: shard %s moved %s → %s", hb.ShardID, existing.Addr, hb.Addr)
		oldShard := existing.Shard
		existing.Shard = gs
		existing.Addr = hb.Addr
		existing.InstanceID = hb.InstanceID
		existing.Suspended = false
		if existing.State == StateSuspended {
			log.Printf("provisioner: shard %s rebound from SUSPENDED → INGESTING", hb.ShardID)
			existing.State = StateIngesting
		}
		p.registry.PersistState(existing)
		// Defer the close: queries that grabbed the old shard.Shard via
		// MatchingShards before the rebind would otherwise see their
		// gRPC stream cancelled mid-flight. Phase 2's retry softens the
		// user-visible impact, but giving the old conn a window to drain
		// is the right primary fix.
		//
		// Nil-guard: LoadFromStore leaves Shard=nil for live (non-
		// archived) entries — the heartbeat-adopt path attaches a
		// GRPCShard later. If the very first heartbeat after restart
		// arrives at a new addr (e.g. instance refresh in flight), the
		// rebind path runs against a nil oldShard. No conn to close.
		if oldShard != nil {
			grace := p.config.CloseRebindGrace
			if grace == 0 {
				grace = defaultCloseRebindGrace
			}
			go func(shardID string, s shard.Shard) {
				time.Sleep(grace)
				if err := s.Close(); err != nil {
					log.Printf("provisioner: deferred close of %s failed: %v", shardID, err)
				}
			}(hb.ShardID, oldShard)
		}
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
		ID:              hb.ShardID,
		Addr:            hb.Addr,
		InstanceID:      hb.InstanceID,
		State:           state,
		RowCount:        hb.RowCount,
		Shard:           gs,
		CreatedAt:       time.Now(),
		LastHeartbeatAt: time.Now(),
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

// christen promotes a LAUNCHING row to INGESTING: finds the row the
// reconciler minted for this instance, sends Configure with the
// row's shard id / window / heartbeat token, and registers the
// entry. Only ever called from ProcessHeartbeat when an unclaimed
// heartbeat arrives — which, since non-async launchers Configure
// synchronously in provisionForBucket, happens only on the async
// (ASG) path. A missing row is an invariant break: the reconciler
// and the hook must agree on which instances are expected, and any
// orphan instance is the stale-LAUNCHING reaper's job.
func (p *Provisioner) christen(hb store.ShardHeartbeat) error {
	// Race guard: two heartbeats from the same unclaimed instance
	// arrive back-to-back (the Heartbeat RPC dispatches each in its
	// own goroutine). heartbeatMu serialises them, but the second
	// goroutine's hb.ShardID is still "" because it was captured at
	// send time, before the first Configure returned. Without this
	// check the second goroutine tries to re-Configure the same
	// addr; the shard rejects with "shard already configured as ...".
	if p.registry.HasAddr(hb.Addr) {
		return nil
	}
	if p.config.ShardOps == nil {
		return fmt.Errorf("christen %s: no ShardOps store — async-launcher handoff requires LAUNCHING-row lookup", hb.Addr)
	}
	if hb.InstanceID == "" {
		return fmt.Errorf("christen %s: heartbeat carried empty instance_id — can't match a LAUNCHING row", hb.Addr)
	}

	row, ok, err := p.config.ShardOps.FindLaunchingByInstance(hb.InstanceID)
	if err != nil {
		return fmt.Errorf("christen %s: find LAUNCHING for instance %s: %w", hb.Addr, hb.InstanceID, err)
	}
	if !ok {
		return fmt.Errorf("christen %s: no LAUNCHING row for instance %s — reconciler/ASG out of sync",
			hb.Addr, hb.InstanceID)
	}

	windowStart, err1 := time.Parse(time.RFC3339, row.WindowStart)
	windowEnd, err2 := time.Parse(time.RFC3339, row.WindowEnd)
	if err1 != nil || err2 != nil || windowStart.IsZero() || windowEnd.IsZero() {
		return fmt.Errorf("christen %s: unparseable window [%s, %s]: %v / %v",
			row.ID, row.WindowStart, row.WindowEnd, err1, err2)
	}

	if p.Buckets == nil {
		return fmt.Errorf("christen %s: no BucketManager — cannot resolve bucket %d", row.ID, row.BucketID)
	}
	bucket, err := p.Buckets.ByID(row.BucketID)
	if err != nil {
		return fmt.Errorf("christen %s: lookup bucket %d: %w", row.ID, row.BucketID, err)
	}
	if bucket == nil {
		return fmt.Errorf("christen %s: bucket %d not found", row.ID, row.BucketID)
	}

	// Fresh bucket.Apps so a shard that LAUNCHED before an
	// OnboardApp call still picks up the new app on promotion. The
	// LAUNCHING row's Apps column is a snapshot for observability;
	// the bucket read is authoritative.
	apps := append([]string(nil), bucket.Apps...)

	req := &pb.ConfigureRequest{
		ShardId:        row.ID,
		Apps:           apps,
		WindowStart:    row.WindowStart,
		WindowEnd:      row.WindowEnd,
		HeartbeatToken: row.HeartbeatToken,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sh, err := p.configurer.Dial(ctx, hb.Addr, req)
	if err != nil {
		return fmt.Errorf("configure %s at %s: %w", row.ID, hb.Addr, err)
	}

	// The lifecycle hook already tagged the volume with row.ID when
	// it claimed the LAUNCHING row (see lifecycle.decide()'s CHRISTEN
	// branch). Look it up so recovery can skip DescribeVolumes; no
	// re-tag necessary.
	var volumeID string
	if p.volumes != nil {
		if info, err := p.volumes.FindByShard(ctx, row.ID); err != nil {
			log.Printf("provisioner: find volume for %s: %v", row.ID, err)
		} else if info != nil {
			volumeID = info.VolumeID
		}
	}

	createdAt, _ := time.Parse(time.RFC3339, row.CreatedAt)
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	p.registry.Add(&ShardEntry{
		ID:              row.ID,
		Apps:            apps,
		WindowStart:     windowStart,
		WindowEnd:       windowEnd,
		State:           StateIngesting,
		Addr:            hb.Addr,
		InstanceID:      hb.InstanceID,
		RowCount:        hb.RowCount,
		EBSVolumeID:     volumeID,
		Shard:           sh,
		CreatedAt:       createdAt,
		HeartbeatToken:  row.HeartbeatToken,
		Tier:            row.Tier,
		BucketID:        row.BucketID,
		LastHeartbeatAt: time.Now(),
	})
	log.Printf("provisioner: christened %s at %s (bucket=%s(%d) window %s–%s volume=%s)",
		row.ID, hb.Addr, bucket.Label, bucket.ID, row.WindowStart, row.WindowEnd, volumeID)
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
		if entry.State == StateArchived || entry.State == StateSuspended {
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
		// volume, its data outlives the instance. Coordinator's
		// explicit play:
		//   1. Preserve the registry entry — rebind needs it to
		//      find the shard post-replacement.
		//   2. Ask the ASG to terminate the dead instance and
		//      launch a replacement (ShouldDecrementDesiredCapacity
		//      = false). The lifecycle hook reattaches the volume
		//      to the replacement; the replacement heartbeats with
		//      the same shard id; the rebind arm of
		//      adoptFromHeartbeats rewires the entry.
		//   3. Null entry.InstanceID so the terminate doesn't
		//      re-fire next tick (the 1–3 minute replacement
		//      window spans multiple ticks). Rebind restores it
		//      from the replacement's heartbeat.
		if entry.EBSVolumeID != "" {
			if entry.InstanceID != "" {
				if rt, ok := p.launcher.(RecoveryTerminator); ok {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					err := rt.TerminateForReplacement(ctx, entry.InstanceID)
					cancel()
					if err != nil {
						log.Printf("provisioner: TerminateForReplacement(%s): %v — will retry next tick",
							entry.InstanceID, err)
					} else {
						log.Printf("provisioner: shard %s stale heartbeat — asked ASG to replace instance %s (vol=%s preserved)",
							entry.ID, entry.InstanceID, entry.EBSVolumeID)
						entry.InstanceID = ""
						entry.Addr = ""
						entry.State = StateSuspended
						entry.LastHeartbeatAt = time.Now()
						p.registry.PersistState(entry)
					}
				} else {
					log.Printf("provisioner: shard %s stale heartbeat — launcher has no RecoveryTerminator; falling back to ASG health-check",
						entry.ID)
				}
			}
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

// bootstrap forces an early reconciler pass on startup so the first
// shards begin launching without waiting a full PollInterval. Just
// runs reconcileBucket for each bucket — the under-capacity op
// emits one LaunchShardOp per missing shard.
func (p *Provisioner) bootstrap(ctx context.Context) {
	if p.Buckets == nil {
		return
	}
	all, err := p.Buckets.All()
	if err != nil {
		log.Printf("provisioner: bootstrap: Buckets.All: %v", err)
		return
	}
	for _, bucket := range all {
		p.ReconcileBucket(ctx, bucket)
	}
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

// OnboardApp records an app as belonging to a bucket and kicks a
// reconciler pass so the new app lands on every INGESTING shard in
// that bucket without waiting for the next ticker. Called from
// AddSource (operator config) and from IngestStream on first-seen
// apps (auto-discovery). For now every app lands in the default
// catch-all bucket; a future Split() can promote individual apps
// into their own buckets.
//
// Idempotent: if the app is already on the bucket, no DB write and
// no reconcile pass. Safe to call on the hot ingest path where
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
	p.ReconcileBucket(context.Background(), updated)
	return nil
}

// check runs one tick of the provisioner's housekeeping: bucket
// split evaluation, per-bucket reconciliation, then an absolute
// ASG desired_capacity sync for each tier.
func (p *Provisioner) check(ctx context.Context) {
	p.registry.RefreshFromStore()

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

	// Per-bucket reconciliation. Covers app-drift (op 1),
	// under-capacity scale-up (op 2), window-closed rotation
	// (op 3), and retention-archive (op 5).
	if p.Buckets != nil {
		all, err := p.Buckets.All()
		if err != nil {
			log.Printf("provisioner: reconcile: Buckets.All: %v", err)
		}
		for _, bucket := range all {
			p.ReconcileBucket(ctx, bucket)
		}
	}

	// Tier-level ASG sync: set each tier's desired_capacity
	// absolutely from bucket config + READ_ONLY registry count.
	// Runs after the per-bucket reconcile so any LAUNCHING rows
	// minted this tick inform the count. No-op on synchronous
	// launchers.
	p.syncASGDesiredCapacity(ctx)
}

// archive tells the shard to export its data to Parquet and replaces
// the live shard with a ColdShard for subsequent queries.
func (p *Provisioner) archive(entry *ShardEntry) {
	if entry.Shard == nil {
		log.Printf("provisioner: cannot archive shard %s: no Shard attached", entry.ID)
		return
	}

	appLabel := joinApps(entry.Apps)
	s3Prefix := fmt.Sprintf("%s/%s", p.config.ParquetDir, appLabel)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	parquetPath, err := entry.Shard.Archive(ctx, s3Prefix)
	if err != nil {
		log.Printf("provisioner: archive %s: %v", entry.ID, err)
		return
	}

	coldShard, err := shard.NewColdShard(entry.ID, entry.Apps, parquetPath, p.config.LambdaFunctionURL, p.config.S3Config)
	if err != nil {
		log.Printf("provisioner: create cold shard for %s: %v", entry.ID, err)
		return
	}
	if p.config.AWSCredentials != nil && p.config.LambdaFunctionURL != "" {
		coldShard.WithAWSAuth(p.config.AWSCredentials, p.config.AWSRegion)
	}

	entry.Shard.Close()
	entry.Shard = coldShard
	entry.State = StateArchived
	entry.ParquetPath = parquetPath
	entry.LastHeartbeatAt = time.Time{}
	p.registry.PersistState(entry)

	log.Printf("provisioner: shard %s archived to %s", entry.ID, parquetPath)

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


// mintLaunchingRow INSERTs a LAUNCHING row for a bucket's current
// window. Thin wrapper over mintLaunchingRowForWindow; see that
// function for the details.
func (p *Provisioner) mintLaunchingRow(bucket *AppBucket) (*store.ShardRecord, error) {
	windowStart, windowEnd := p.currentWindow(bucket, time.Now().UTC())
	return p.mintLaunchingRowForWindow(bucket, windowStart, windowEnd)
}

// mintLaunchingRowForWindow INSERTs a LAUNCHING row for a bucket
// with caller-supplied window bounds. Called with the current
// window from op 2's under-capacity path and with the next window
// from op 4's pre-warm path. The row's shard id and heartbeat
// token are minted now; instance_id/addr stay empty until the
// lifecycle hook + christen fill them in.
func (p *Provisioner) mintLaunchingRowForWindow(bucket *AppBucket, windowStart, windowEnd time.Time) (*store.ShardRecord, error) {
	if p.config.ShardOps == nil {
		return nil, fmt.Errorf("no ShardOps store configured")
	}

	now := time.Now().UTC()

	// 32-bit random suffix avoids shard-id collisions across
	// concurrent ticks and across buckets on the same window. The
	// old registry-size-based seq (shard-<window>-N) didn't see
	// LAUNCHING rows and could re-emit an existing id.
	suf := make([]byte, 4)
	if _, err := rand.Read(suf); err != nil {
		return nil, fmt.Errorf("mint shard id suffix: %w", err)
	}
	shardID := fmt.Sprintf("shard-%s-%s", windowStart.Format("20060102T1504"), hex.EncodeToString(suf))

	token, err := store.GenerateShardHeartbeatToken()
	if err != nil {
		return nil, fmt.Errorf("mint heartbeat token for %s: %w", shardID, err)
	}

	tier := bucket.Tier
	if tier == "" {
		tier = "small"
	}

	apps := append([]string(nil), bucket.Apps...)
	appsJSON, _ := json.Marshal(apps)

	rec := &store.ShardRecord{
		ID:             shardID,
		Apps:           string(appsJSON),
		WindowStart:    windowStart.Format(time.RFC3339),
		WindowEnd:      windowEnd.Format(time.RFC3339),
		State:          string(StateLaunching),
		Tier:           tier,
		CreatedAt:      now.Format(time.RFC3339),
		BucketID:       bucket.ID,
		HeartbeatToken: token,
	}
	if err := p.config.ShardOps.SaveShard(rec); err != nil {
		return nil, fmt.Errorf("INSERT LAUNCHING row for bucket %d: %w", bucket.ID, err)
	}
	log.Printf("provisioner: minted LAUNCHING row %s for bucket %s(%d) (tier=%s window %s–%s)",
		shardID, bucket.Label, bucket.ID, tier,
		windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
	return rec, nil
}

// provisionForBucket launches a new INGESTING shard for a bucket.
// Tier comes from the bucket record so per-bucket fleet sizing (small
// catch-all vs. medium dedicated, etc.) is data-driven.
func (p *Provisioner) provisionForBucket(ctx context.Context, bucket *AppBucket) {
	tier := bucket.Tier
	if tier == "" {
		tier = "small"
	}

	if isLaunchingLauncher(p.launcher) {
		// INSERT the LAUNCHING row. The ASG scale-up happens out of
		// band via syncASGDesiredCapacity at the end of this tick,
		// which reads bucket targets and adjusts desired_capacity
		// absolutely. No per-launch SetDesiredCapacity bumps.
		if p.config.ShardOps == nil {
			log.Printf("provisioner: bucket %d: no ShardOps store — skipping LAUNCHING mint (async handoff requires it)", bucket.ID)
			return
		}
		if _, err := p.mintLaunchingRow(bucket); err != nil {
			log.Printf("provisioner: bucket %d: mint LAUNCHING: %v", bucket.ID, err)
		}
		return
	}

	// Synchronous launcher: mint a window aligned to the bucket's
	// WindowDuration boundary, launch + Configure inline, register
	// the entry straight into INGESTING. No LAUNCHING-row dance —
	// the launcher returns only after the shard is ready.
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
