package logdb

import (
	"context"
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

// HeartbeatStore is the interface the provisioner needs for heartbeat operations.
type HeartbeatStore interface {
	LiveShardHeartbeats(since string) ([]store.ShardHeartbeat, error)
	InsertHeartbeat(hb *store.ShardHeartbeat) error
}

// ShardOpsStore is the shard_records read/stamp surface the provisioner
// needs beyond plain persistence — the read-routing service lookups and
// the bucket-service membership reads. (The old LAUNCHING-row lifecycle
// methods are gone; the FSM mints + persists rows directly.) Separate
// from HeartbeatStore so tests that don't exercise this path can keep
// their minimal mocks. *store.Store satisfies it natively.
type ShardOpsStore interface {
	SaveShard(rec *store.ShardRecord) error
	DeleteShard(id string) error
	GetShard(id string) (store.ShardRecord, bool, error)
	// StampShardServices records the services the shard's bucket serves
	// into shard_services — the permanent stamp future queries resolve
	// through (stamped at mint).
	StampShardServices(shardID string, bucketID int64) error
	// ShardIDsServingService returns the set of shard IDs serving
	// (namespaceID, service) — the read-routing lookup ShardDescriptorsFor
	// resolves before filtering the registry. An empty service narrows to
	// the namespace only.
	ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error)
	// ServicesForBucket returns the (namespace_id, service) pairs a bucket
	// serves (from log_groups). The service-drift sweep diffs it against
	// each INGESTING shard's stamp to add services onboarded after mint.
	ServicesForBucket(bucketID int64) ([]store.ShardService, error)
}

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
	volumes     VolumeManager // optional; the shard SM creates/attaches/retires EBS volumes through it
	heartbeatMu sync.Mutex    // serialises ProcessHeartbeat with the ticker's adoptFromHeartbeats

	// bucketSMs / shardSMs are the FSM control plane (asg-as-api-stateless.md).
	// LoadStateMachines populates them after the registry hydrates;
	// MintShard adds to shardSMs as the bucket SMs warm new cohorts.
	// check() drives them every tick — they own all lifecycle convergence.
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

// SetVolumeManager wires the EBS VolumeManager the shard SM uses to
// create/attach volumes on launch and snapshot/detach/delete them on
// archive. Optional — dev mode (no EBS) leaves it nil.
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
