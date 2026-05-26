package logdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// ErrShardAuthFailed marks an AuthenticateShard failure that the
// caller should surface as gRPC Unauthenticated (unknown shard,
// no-token shard, or mismatched bearer). Lookup errors — e.g. a
// store failure on the registry-miss fallback path — are returned
// raw so the caller can map them to Internal.
var ErrShardAuthFailed = errors.New("shard heartbeat authentication failed")

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
	GetShard(id string) (store.ShardRecord, bool, error)
	ClaimLaunchingForTier(tier, instanceID string) (store.ShardRecord, bool, error)
	FindLaunchingByInstance(instanceID string) (store.ShardRecord, bool, error)
	ListStaleLaunching(olderThan time.Duration) ([]store.ShardRecord, error)
	CountShardsInBucket(bucketID int64, states []string) (int, error)
	CountShardsInBucketForFutureWindows(bucketID int64, currentWindowStart time.Time) (int, error)
	// StampShardServices records (at christen) the services the shard's
	// bucket serves into shard_services — the permanent stamp future
	// queries resolve through.
	StampShardServices(shardID string, bucketID int64) error
	// ShardIDsServingService returns the set of shard IDs serving
	// (namespaceID, service) — the read-routing lookup ShardDescriptorsFor
	// resolves before filtering the registry. An empty service narrows to
	// the namespace only.
	ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error)
	// ServicesForBucket returns the (namespace_id, service) pairs a
	// bucket serves (from log_groups). christen uses it to populate a
	// shard's in-memory service membership; the reconciler diffs it to
	// find services onboarded after christen.
	ServicesForBucket(bucketID int64) ([]store.ShardService, error)
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
	PollInterval      time.Duration  // how often to check (default 5min)
	ParquetDir        string         // directory for Parquet exports (local path or s3:// prefix)
	LambdaFunctionURL string         // Lambda function URL for cold queries (empty = in-process ColdShard)
	Store             HeartbeatStore // for reading shard heartbeats (nil = no heartbeat adoption)
	WindowDuration    time.Duration  // duration of a single shard window (w.duration in docs/shard-lifecycle.md)
	ConfigureRetries  int            // attempts to Configure a freshly-launched shard (default 12)
	ConfigureBackoff  time.Duration  // sleep between Configure attempts (default 5s)
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

// ServiceStats tracks per-app ingestion rates for adaptive bucket splitting.
type ServiceStats struct {
	mu     sync.Mutex
	counts map[string]*atomic.Int64
}

// NewServiceStats creates a new stats tracker.
func NewServiceStats() *ServiceStats {
	return &ServiceStats{
		counts: make(map[string]*atomic.Int64),
	}
}

// Record increments the row count for an app.
func (s *ServiceStats) Record(app string, n int64) {
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
func (s *ServiceStats) Snapshot() map[string]int64 {
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
	stats       *ServiceStats
	Buckets     *BucketManager
	configurer  ShardConfigurer
	volumes     VolumeManager // optional; used to tag data volumes at christen time
	heartbeatMu sync.Mutex    // serialises ProcessHeartbeat with the ticker's adoptFromHeartbeats

	// bucketSMs / shardSMs are the FSM control plane built additively in
	// Slice C (asg-as-api-stateless.md). LoadStateMachines populates them
	// after the registry hydrates; MintShard adds to shardSMs as the
	// bucket SMs warm new cohorts. check() does NOT drive them yet — the
	// old reconciler stays authoritative until the Slice C switch commit.
	smMu      sync.RWMutex
	bucketSMs map[int64]*BucketSM
	shardSMs  map[string]*ShardSM
	// store is the concrete *store.Store behind config.Store/ShardOps,
	// stashed by SetStore. The FSM control plane needs the concrete type
	// (ShardSMDeps.Store, the bucket SM's *gorm.DB) that the HeartbeatStore
	// / ShardOpsStore interfaces don't expose.
	store *store.Store
}

// NewProvisioner creates a provisioner.
func NewProvisioner(registry *ShardRegistry, launcher ShardLauncher, config ProvisionerConfig) *Provisioner {
	return &Provisioner{
		registry:   registry,
		launcher:   launcher,
		config:     config,
		stats:      NewServiceStats(),
		Buckets:    NewBucketManager(nil),
		configurer: grpcConfigurer{shardSecret: config.ShardSecret},
		bucketSMs:  map[int64]*BucketSM{},
		shardSMs:   map[string]*ShardSM{},
	}
}

// SetStore provides the store for heartbeat and LAUNCHING-row
// operations. Called after the store is opened (which may happen
// after the provisioner starts). *store.Store satisfies both
// HeartbeatStore and ShardOpsStore, so one call wires both — and it
// also (re)builds the BucketManager against the live store, so
// callers no longer wire buckets in a separate step.
func (p *Provisioner) SetStore(s *store.Store) {
	p.config.Store = s
	p.config.ShardOps = s
	p.store = s
	p.Buckets = NewBucketManager(s)
}

// SetVolumeManager enables volume-aware christening. When set, the
// provisioner stamps the minted shard id onto the pre-attached EBS
// volume as the final step of christen().
func (p *Provisioner) SetVolumeManager(vm VolumeManager) {
	p.volumes = vm
}

// Stats returns the provisioner's app stats tracker. The drain loop
// should call stats.Record() after each successful ingestion.
func (p *Provisioner) Stats() *ServiceStats {
	return p.stats
}

// Registry-shaped methods. The ShardRegistry is a private field; the
// methods below are the coordinator-facing surface that keeps the
// shard-data control plane behind one boundary.
//
// IngestingShard / TotalIngesting / RecordIngestFailure /
// ResetIngestFailures are thin pass-throughs (the last two also
// satisfy shard.IngestRecorder so drain can pass *Provisioner
// straight into sh.Ingest). ShardDescriptorsFor is the one
// exception — it folds the entry→descriptor transform into the
// boundary so *ShardEntry never leaks across, and the registry
// stays free of proto knowledge.

// IngestingShard returns an INGESTING shard serving (namespaceID,
// service) via round-robin across matching shards, or nil if none
// qualify.
func (p *Provisioner) IngestingShard(namespaceID uint, service string) shard.Shard {
	return p.registry.IngestingShard(namespaceID, service)
}

// TotalIngesting returns the count of INGESTING shards regardless
// of app. Used by the disk-pressure check.
func (p *Provisioner) TotalIngesting() int { return p.registry.TotalIngesting() }

// RecordIngestFailure increments the consecutive-failure counter for
// id and may flip the entry to Suspended once the threshold trips.
// Returns (count, threshold). Half of the shard.IngestRecorder
// contract.
func (p *Provisioner) RecordIngestFailure(id string) (count, threshold int) {
	return p.registry.RecordIngestFailure(id)
}

// ResetIngestFailures clears the consecutive-failure counter for id.
// Other half of shard.IngestRecorder, called by Shard.Ingest on the
// success path.
func (p *Provisioner) ResetIngestFailures(id string) {
	p.registry.ResetIngestFailures(id)
}

// ShardDescriptorsFor enumerates shards that intersect tr and serve the
// queried (namespaceID, service), then builds the proto ShardDescriptor
// each compute worker needs to dial or restore them. The serving set is
// resolved from shard_services (the christen-time stamp) here, then
// handed to the registry's pure MatchingEntries. tier clamps the time
// range to the caller's retention window before matching. The
// provisioner's own LambdaFunctionURL (set from $LAMBDA_FUNCTION_URL in
// logdb.Start) is stamped onto archived entries for the cold-shard
// restore path; live entries get the gRPC address instead.
//
// namespaceID == 0 means "unresolved / no namespace filter" — every
// shard with an overlapping window matches, preserving the old app==""
// fan-out (a bare /log/warp.log, or a namespace the customer can't
// resolve). A non-zero namespaceID with an empty service narrows to the
// namespace (all its services). A serving lookup error logs and falls
// back to no filter rather than silently returning zero shards.
//
// Returns an empty slice if nothing matches — callers can range over
// it without nil-checking. This is the only path coordinator-side
// code uses to read shard state for queries.
func (p *Provisioner) ShardDescriptorsFor(tr TimeRange, namespaceID uint, service string, tier store.Tier) []*pb.ShardDescriptor {
	var serving map[string]struct{}
	if namespaceID != 0 && p.config.ShardOps != nil {
		s, err := p.config.ShardOps.ShardIDsServingService(namespaceID, service)
		if err != nil {
			log.Printf("provisioner: ShardDescriptorsFor: serving lookup ns=%d service=%q: %v — falling back to no filter", namespaceID, service, err)
		} else {
			serving = s
		}
	}
	entries := p.registry.MatchingEntries(tr, serving, tier)
	out := make([]*pb.ShardDescriptor, len(entries))
	for i, e := range entries {
		d := &pb.ShardDescriptor{ShardId: e.ID, Services: serviceNames(e.Services)}
		if e.State == StateArchived {
			d.LambdaUrl = p.config.LambdaFunctionURL
			d.ParquetPath = e.ParquetPath
		} else {
			d.GrpcAddr = e.Addr
		}
		out[i] = d
	}
	return out
}

// Start runs the provisioner loop until the context is cancelled.
// It ensures at least one INGESTING shard exists on startup.
func (p *Provisioner) Start(ctx context.Context) {
	go func() {
		// First pass immediately so shards begin launching without waiting
		// a full PollInterval: adoptFromHeartbeats catches up any heartbeats
		// already in the store (driving promotion/liveness through the SMs),
		// then check() runs one FSM convergence tick — which mints and
		// launches toward each bucket's target. No separate bootstrap or
		// reap step: the bucket/shard SMs own capacity and failure handling.
		p.adoptFromHeartbeats()
		p.check(ctx)

		ticker := time.NewTicker(p.config.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.adoptFromHeartbeats()
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
func (p *Provisioner) syncASGDesiredCapacity(ctx context.Context, all []*Bucket) {
	scaler, ok := p.launcher.(TierScaler)
	if !ok {
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

// reapStrandedSuspended deletes SUSPENDED shard rows whose rebind
// never completed, but ONLY when deleting the row cannot lose data
// that isn't already lost or preserved elsewhere:
//
//   - FindByShard(entry.ID) returns nil → the volume is either gone
//     (data already lost — the row is a tombstone; snapshots are
//     intentionally out of scope) or its shard_id tag moved to a
//     different shard (data preserved under that shard). Either way
//     deleting this row loses nothing → reap it.
//   - FindByShard returns a volume → it still exists tagged to THIS
//     shard, i.e. this row is the sole pointer to live data. Leave it
//     for the rebind path (this is the e.g. 822eab40 / 164k-row case).
//   - FindByShard errors → unknown; skip and retry next tick (fail
//     safe — never delete on uncertainty).
//
// Non-SUSPENDED states are out of scope (reapDeadShards owns the
// dead-but-not-yet-suspended transition; this only cleans up the
// terminal stranded case it leaves behind).
func (p *Provisioner) reapStrandedSuspended() {
	if p.volumes == nil || p.config.ShardOps == nil {
		return
	}
	for _, entry := range p.registry.Entries() {
		if entry.State != StateSuspended {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		vi, err := p.volumes.FindByShard(ctx, entry.ID)
		cancel()
		if err != nil {
			log.Printf("provisioner: reap-suspended %s: volume lookup failed (%v) — skipping", entry.ID, err)
			continue
		}
		if vi != nil {
			// Volume present and still tagged to this shard — sole
			// pointer to live data. Leave it for rebind.
			continue
		}
		log.Printf("provisioner: reaping stranded SUSPENDED %s (vol=%q gone or re-homed — data not lost by this deletion)",
			entry.ID, entry.EBSVolumeID)
		p.registry.Remove(entry.ID)
		if err := p.config.ShardOps.DeleteShard(entry.ID); err != nil {
			log.Printf("provisioner: delete stranded SUSPENDED %s: %v", entry.ID, err)
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

// AuthenticateShard validates the bearer presented for shardID
// against the heartbeat token the provisioner minted at christen.
// Lookup order is registry → store: a coordinator restart leaves the
// in-memory registry empty for live shards (LoadFromStore filters to
// ARCHIVED), so the first heartbeat from a returning shard has to
// validate against the persisted ShardRecord.
//
// Returns nil on match. On auth misses (unknown shard, no-token
// shard, mismatched bearer) returns an error wrapping
// ErrShardAuthFailed; on a store-side lookup failure returns the
// raw error so the caller can distinguish auth-deny from infra
// trouble.
func (p *Provisioner) AuthenticateShard(shardID, presented string) error {
	expectedToken := ""
	if entry := p.registry.Get(shardID); entry != nil {
		expectedToken = entry.HeartbeatToken
	} else if p.config.ShardOps != nil {
		rec, ok, err := p.config.ShardOps.GetShard(shardID)
		if err != nil {
			return fmt.Errorf("lookup shard %s: %w", shardID, err)
		}
		if ok {
			expectedToken = rec.HeartbeatToken
		}
	}
	if expectedToken == "" {
		return fmt.Errorf("%w: unknown or untokened shard %s", ErrShardAuthFailed, shardID)
	}
	if !store.ConstantTimeTokenEqual(expectedToken, presented) {
		return fmt.Errorf("%w: bearer mismatch for %s", ErrShardAuthFailed, shardID)
	}
	return nil
}

// ProcessHeartbeat routes one heartbeat to its shard SM. Exported so the
// Heartbeat RPC handler drives transitions immediately rather than wait a
// tick; also called from adoptFromHeartbeats as a catch-up pass.
// Serialised against itself via heartbeatMu so two concurrent heartbeats
// (the RPC and a tick) for the same shard can't interleave the
// instance-correlation lookup.
//
// Three cases:
//   - empty ShardID  → a freshly-booted instance that hasn't been
//     Configured. Correlate to the LAUNCHING shard SM the bucket minted
//     (by the instance id the launcher stamped) and fire Heartbeat — its
//     OnEntryFrom dials + Configures, assigning the shard its identity.
//   - known id, LAUNCHING/SUSPENDED → fire Heartbeat (promote or rebind).
//   - known id, steady state → ObserveHeartbeat: refresh liveness and
//     reattach the gRPC handle if it's missing (post-restart) or moved.
func (p *Provisioner) ProcessHeartbeat(hb store.ShardHeartbeat) {
	p.heartbeatMu.Lock()
	defer p.heartbeatMu.Unlock()
	ctx := context.Background()

	if hb.ShardID == "" {
		sm := p.shardSMByInstance(hb.InstanceID)
		if sm == nil {
			// No LAUNCHING shard is awaiting this instance — a stray
			// unclaimed heartbeat (orphan instance, or one already
			// promoted). The stale-LAUNCHING path reaps genuine orphans.
			return
		}
		if err := sm.Fire(ctx, TriggerHeartbeat, hb); err != nil {
			log.Printf("provisioner: heartbeat promote (instance %s → %s): %v", hb.InstanceID, sm.ID(), err)
		}
		return
	}

	p.smMu.RLock()
	sm := p.shardSMs[hb.ShardID]
	p.smMu.RUnlock()
	if sm == nil {
		// Unknown shard id. LoadStateMachines builds an SM per persisted
		// row at boot, so a miss means a shard we have no record of —
		// nothing to drive.
		return
	}

	state := StateIngesting
	if e := p.registry.Get(hb.ShardID); e != nil {
		state = e.State
	}
	switch state {
	case StateLaunching, StateSuspended:
		if err := sm.Fire(ctx, TriggerHeartbeat, hb); err != nil {
			log.Printf("provisioner: heartbeat fire %s: %v", hb.ShardID, err)
		}
	default:
		sm.ObserveHeartbeat(ctx, hb)
	}
}

// shardSMByInstance finds the LAUNCHING shard SM whose entry carries the
// given instance id — the correlation an empty-ShardID heartbeat needs to
// reach the shard the bucket minted for this instance. Only LAUNCHING
// entries are candidates: a promoted shard heartbeats with its real id and
// routes by id instead.
func (p *Provisioner) shardSMByInstance(instanceID string) *ShardSM {
	if instanceID == "" {
		return nil
	}
	for _, e := range p.registry.Entries() {
		if e.State == StateLaunching && e.InstanceID == instanceID {
			p.smMu.RLock()
			sm := p.shardSMs[e.ID]
			p.smMu.RUnlock()
			return sm
		}
	}
	return nil
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

	// The shard's service membership, read from the bucket's log_groups —
	// the same source StampShardServices stamps into shard_services
	// below, so the in-memory set and the persisted stamp agree. A shard
	// that LAUNCHED before a service was onboarded picks it up here on
	// promotion (the bucket read is authoritative, not the LAUNCHING-row
	// snapshot).
	svcs, err := p.config.ShardOps.ServicesForBucket(row.BucketID)
	if err != nil {
		return fmt.Errorf("christen %s: services for bucket %d: %w", row.ID, row.BucketID, err)
	}

	req := &pb.ConfigureRequest{
		ShardId:        row.ID,
		Services:       serviceNames(svcs),
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
		Services:        svcs,
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
	// Stamp the shard with its bucket's services (shard_services,
	// migration 058) — the permanent snapshot the read router resolves
	// through (ShardDescriptorsFor → ShardIDsServingService). Reliable,
	// not best-effort: now that query routing reads shard_services
	// exclusively, an unstamped shard is invisible to every query for
	// its services, so a stamp failure must fail christen. The shard
	// stays a LAUNCHING row and gets re-christened on the next heartbeat
	// rather than silently entering INGESTING as a query black hole.
	if err := p.config.ShardOps.StampShardServices(row.ID, row.BucketID); err != nil {
		return fmt.Errorf("christen %s: stamp shard_services: %w", row.ID, err)
	}

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
func (p *Provisioner) currentWindow(bucket *Bucket, now time.Time) (time.Time, time.Time) {
	dur := p.windowDurationFor(bucket)
	start := now.UTC().Truncate(dur)
	return start, start.Add(dur)
}

// windowDurationFor picks the effective WindowDuration for a bucket,
// with a two-step fallback so tests and boot-before-hydration paths
// stay working. Production paths always pass a bucket with a set
// WindowDuration (persisted by migration 021).
func (p *Provisioner) windowDurationFor(bucket *Bucket) time.Duration {
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
func (p *Provisioner) launchAndConfigure(ctx context.Context, shardID, tier string, svcs []store.ShardService, windowStart, windowEnd time.Time) (*ShardEntry, error) {
	log.Printf("provisioner: launching shard %s (tier=%s)", shardID, tier)

	addr, err := p.launcher.LaunchInstance(ctx, tier)
	if err != nil {
		return nil, fmt.Errorf("launch: %w", err)
	}

	// Mint a heartbeat token and thread it through both the
	// ConfigureRequest (so the shard sends it on every heartbeat) and
	// the registered ShardEntry (so authenticateHeartbeat has something
	// to compare against). The LAUNCHING-row christen path mints its
	// token at row-insert time; for the synchronous launcher we do it
	// inline. Pre-2026-04-27, heartbeat auth had a grandfather clause
	// that accepted any bearer for shards with empty HeartbeatToken,
	// so this path got away without a token; that grandfather is gone
	// (c1629b2) and a missing token now fails every heartbeat with
	// "unknown or untokened shard".
	heartbeatToken, err := store.GenerateShardHeartbeatToken()
	if err != nil {
		p.launcher.TerminateInstance(ctx, "", addr)
		return nil, fmt.Errorf("mint heartbeat token for %s: %w", shardID, err)
	}

	cfg := &pb.ConfigureRequest{
		ShardId:        shardID,
		Services:       serviceNames(svcs),
		WindowStart:    windowStart.Format(time.RFC3339),
		WindowEnd:      windowEnd.Format(time.RFC3339),
		HeartbeatToken: heartbeatToken,
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
		ID:             shardID,
		Services:       svcs,
		WindowStart:    windowStart,
		WindowEnd:      windowEnd,
		State:          StateIngesting,
		Addr:           addr,
		Shard:          sh,
		HeartbeatToken: heartbeatToken,
		CreatedAt:      time.Now(),
	}, nil
}

// OnboardService records an app as belonging to a bucket and kicks a
// reconciler pass so the new app lands on every INGESTING shard in
// that bucket without waiting for the next ticker. Called from
// AddSource (operator config) and from IngestStream on first-seen
// apps (auto-discovery). For now every app lands in the default
// catch-all bucket; a future Split() can promote individual apps
// into their own buckets.
//
// Idempotent: if the app is already on the bucket, no DB write and
// no reconcile pass. Safe to call on the hot ingest path where
// SeenServices.Add returns true for every row the coordinator has ever
// seen from a given (customer, app) pair.
func (p *Provisioner) OnboardService(app string) error {
	if p.Buckets == nil {
		return nil
	}
	bucket, err := p.Buckets.Default()
	if err != nil {
		return fmt.Errorf("OnboardService(%s): lookup default: %w", app, err)
	}
	if bucket == nil {
		log.Printf("provisioner: OnboardService(%s): no default bucket — skipping", app)
		return nil
	}
	updated, err := p.Buckets.AddServiceToBucket(bucket.ID, app)
	if err != nil {
		return fmt.Errorf("add %q to bucket %d: %w", app, bucket.ID, err)
	}
	if updated == nil {
		return nil
	}
	// Immediate propagation: land the new service on the bucket's live
	// INGESTING shards now rather than waiting for the next tick's sweep.
	p.syncServicesForBucket(updated)
	return nil
}

// check runs one tick of FSM-driven convergence: refresh the registry
// from the store, evaluate bucket splits, sweep service-drift, then drive
// every bucket and shard SM toward its target. The SMs own all lifecycle
// transitions — launch, rotate, archive, recover — so check() no longer
// emits reconciler ops or syncs ASG desired_capacity (the per-AZ
// ASGProvisioner moves capacity one instance at a time inside Launch).
func (p *Provisioner) check(ctx context.Context) {
	p.registry.RefreshFromStore()

	// Bucket-split evaluation stays — it's orthogonal to the lifecycle
	// cascade (it carves a hot app into its own bucket; the new bucket's
	// SM then warms shards for it on the next tick).
	if p.Buckets != nil {
		snapshot := p.stats.Snapshot()
		if app, err := p.Buckets.EvaluateSplit(snapshot); err != nil {
			log.Printf("provisioner: EvaluateSplit: %v", err)
		} else if app != "" {
			if bucket, err := p.Buckets.Split(app); err != nil {
				log.Printf("provisioner: Split(%s): %v", app, err)
			} else if bucket != nil {
				log.Printf("provisioner: split %s into dedicated bucket %d", app, bucket.ID)
			}
		}
	}

	// Service-drift: a service onboarded after a shard christened isn't on
	// its stamp yet. The FSM cascade is deliberately lifecycle-only, so
	// this membership sweep (the old reconciler's op 1) stays a standalone
	// pass rather than a bucket-SM concern.
	p.syncShardServices()

	// FSM convergence — buckets first (cascade directives + minting), then
	// shards (their own launch/stale/recovery/archive work).
	p.evaluateStateMachines(ctx)
}

// evaluateStateMachines drives every bucket SM then every shard SM one
// tick toward convergence. Buckets run first: a WARMING bucket mints new
// shard SMs, and re-snapshotting after the bucket pass lets those minted
// shards Evaluate the same tick (their launch starts now, not next tick).
func (p *Provisioner) evaluateStateMachines(ctx context.Context) {
	p.ensureBucketSMs()

	p.smMu.RLock()
	buckets := make([]*BucketSM, 0, len(p.bucketSMs))
	for _, b := range p.bucketSMs {
		buckets = append(buckets, b)
	}
	p.smMu.RUnlock()
	for _, b := range buckets {
		if err := b.Evaluate(ctx); err != nil {
			log.Printf("provisioner: bucket %d Evaluate: %v", b.ID, err)
		}
	}

	p.smMu.RLock()
	shards := make([]*ShardSM, 0, len(p.shardSMs))
	for _, s := range p.shardSMs {
		shards = append(shards, s)
	}
	p.smMu.RUnlock()
	for _, s := range shards {
		if err := s.Evaluate(ctx); err != nil {
			log.Printf("provisioner: shard %s Evaluate: %v", s.ID(), err)
		}
	}
}

// ensureBucketSMs keeps the bucketSMs map current with the bucket set:
// LoadStateMachines seeds it at boot, and a bucket created mid-run (a
// Split) gets its SM here. Existing SMs have their embedded *Bucket
// refreshed so a target/window config change is picked up (reconfiguring
// a bucket mid-cascade is otherwise undefined — see plan non-goals).
// Runs inside the serial tick, so no Evaluate races the reassignment.
func (p *Provisioner) ensureBucketSMs() {
	if p.Buckets == nil || p.store == nil {
		return
	}
	all, err := p.Buckets.All()
	if err != nil {
		log.Printf("provisioner: ensureBucketSMs: Buckets.All: %v", err)
		return
	}
	p.smMu.Lock()
	defer p.smMu.Unlock()
	for _, bk := range all {
		if sm, ok := p.bucketSMs[bk.ID]; ok {
			sm.Bucket = bk
		} else {
			p.bucketSMs[bk.ID] = NewBucketSM(bk, p.store.DB(), p, nil)
		}
	}
}

// syncShardServices is the standalone service-drift sweep (old reconciler
// op 1): every INGESTING shard must serve every (namespace, service) its
// bucket serves. AddServiceToShard is idempotent and persists to
// shard_services, so an already-present membership is a cheap no-op.
func (p *Provisioner) syncShardServices() {
	if p.Buckets == nil {
		return
	}
	all, err := p.Buckets.All()
	if err != nil {
		log.Printf("provisioner: syncShardServices: Buckets.All: %v", err)
		return
	}
	for _, bucket := range all {
		p.syncServicesForBucket(bucket)
	}
}

// syncServicesForBucket applies op-1 service-drift to one bucket's
// INGESTING shards. Also the immediate-propagation hook OnboardService
// calls so a freshly-onboarded service reaches live shards without
// waiting for the next tick.
func (p *Provisioner) syncServicesForBucket(bucket *Bucket) {
	if bucket == nil || p.config.ShardOps == nil {
		return
	}
	svcs, err := p.config.ShardOps.ServicesForBucket(bucket.ID)
	if err != nil {
		log.Printf("provisioner: services for bucket %d: %v", bucket.ID, err)
		return
	}
	for _, e := range p.registry.Entries() {
		if e.BucketID != bucket.ID || e.State != StateIngesting {
			continue
		}
		for _, s := range svcs {
			p.registry.AddServiceToShard(e.ID, s.NamespaceID, s.Service)
		}
	}
}

// archive tells the shard to export its data to Parquet and replaces
// the live shard with a ColdShard for subsequent queries.
// ForceArchive is the admin-facing entry point for archiving a
// READ_ONLY shard on demand, bypassing the reconciler's retention
// window check. Returns an error if the shard can't be archived.
func (p *Provisioner) ForceArchive(shardID string) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s not found in registry", shardID)
	}
	if entry.State != StateReadOnly {
		return fmt.Errorf("shard %s is %s, not READ_ONLY", shardID, entry.State)
	}
	p.archive(entry)
	if entry.State != StateArchived {
		return fmt.Errorf("archive failed for shard %s", shardID)
	}
	return nil
}

// DeleteArchived removes an archived shard from the registry and
// deletes its DB record. The Parquet data on S3 is left in place
// (tombstone cleanup is a separate concern).
func (p *Provisioner) DeleteArchived(shardID string) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s not found in registry", shardID)
	}
	if entry.State != StateArchived {
		return fmt.Errorf("shard %s is %s, not ARCHIVED", shardID, entry.State)
	}
	if entry.Shard != nil {
		entry.Shard.Close()
	}
	p.registry.Remove(shardID)
	if err := p.config.ShardOps.DeleteShard(shardID); err != nil {
		return fmt.Errorf("delete shard record: %w", err)
	}
	log.Printf("provisioner: archived shard %s deleted", shardID)
	return nil
}

func (p *Provisioner) archive(entry *ShardEntry) {
	if entry.Shard == nil {
		log.Printf("provisioner: cannot archive shard %s: no Shard attached", entry.ID)
		return
	}

	appLabel := joinServices(serviceNames(entry.Services))
	s3Prefix := fmt.Sprintf("%s/%s", p.config.ParquetDir, appLabel)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	parquetPath, err := entry.Shard.Archive(ctx, s3Prefix)
	if err != nil {
		log.Printf("provisioner: archive %s: %v", entry.ID, err)
		return
	}

	// ColdShard built here is dead inventory — the live query path
	// runs in compute, which dials its own ColdShard from the
	// descriptor. We still slot it into entry.Shard so registry
	// walks see a non-nil pointer for the archived entry.
	coldShard, err := shard.NewColdShard(entry.ID, serviceNames(entry.Services), parquetPath, p.config.LambdaFunctionURL, nil)
	if err != nil {
		log.Printf("provisioner: create cold shard for %s: %v", entry.ID, err)
		return
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
func (p *Provisioner) mintLaunchingRow(bucket *Bucket) (*store.ShardRecord, error) {
	windowStart, windowEnd := p.currentWindow(bucket, time.Now().UTC())
	return p.mintLaunchingRowForWindow(bucket, windowStart, windowEnd)
}

// mintLaunchingRowForWindow INSERTs a LAUNCHING row for a bucket
// with caller-supplied window bounds. Called with the current
// window from op 2's under-capacity path and with the next window
// from op 4's pre-warm path. The row's shard id and heartbeat
// token are minted now; instance_id/addr stay empty until the
// lifecycle hook + christen fill them in.
func (p *Provisioner) mintLaunchingRowForWindow(bucket *Bucket, windowStart, windowEnd time.Time) (*store.ShardRecord, error) {
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

	rec := &store.ShardRecord{
		ID:             shardID,
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
func (p *Provisioner) provisionForBucket(ctx context.Context, bucket *Bucket) {
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
	id := fmt.Sprintf("shard-%s-%s-%d", joinServices(bucket.Services), windowStart.Format("20060102T1504"), seq)

	var svcs []store.ShardService
	if p.config.ShardOps != nil {
		var err error
		if svcs, err = p.config.ShardOps.ServicesForBucket(bucket.ID); err != nil {
			log.Printf("provisioner: bucket %d: services for bucket: %v", bucket.ID, err)
			return
		}
	}

	entry, err := p.launchAndConfigure(ctx, id, tier, svcs, windowStart, windowEnd)
	if err != nil {
		log.Printf("provisioner: failed to launch shard for bucket %d: %v", bucket.ID, err)
		return
	}
	entry.BucketID = bucket.ID
	entry.Tier = tier

	p.registry.Add(entry)
	// Stamp shard_services so read routing (ShardIDsServingService) finds
	// this shard — the sync-launcher analogue of christen's stamp.
	if p.config.ShardOps != nil {
		if err := p.config.ShardOps.StampShardServices(id, bucket.ID); err != nil {
			log.Printf("provisioner: shard %s: stamp shard_services: %v", id, err)
		}
	}
	log.Printf("provisioner: shard %s launched for bucket %d", id, bucket.ID)
}

// serviceNames renders a shard's service membership as bare service
// strings — for the Configure RPC (informational; the shard only logs
// it), the cold-shard label, and ShardDescriptor.Apps (dead metadata the
// cold-query path ignores). Deduplicated: a namespace can repeat a
// service name only across namespaces, but the same (ns, service) never
// appears twice, so the values are already distinct per entry.
func serviceNames(svcs []store.ShardService) []string {
	if len(svcs) == 0 {
		return nil
	}
	out := make([]string, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, s.Service)
	}
	return out
}

func joinServices(apps []string) string {
	if len(apps) == 0 {
		return "shared"
	}
	if len(apps) == 1 {
		return apps[0]
	}
	return fmt.Sprintf("%d-apps", len(apps))
}
