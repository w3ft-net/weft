package logdb

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// ErrShardAuthFailed marks an AuthenticateShard failure that the
// caller should surface as gRPC Unauthenticated (unknown shard,
// no-token shard, or mismatched bearer). Lookup errors — e.g. a
// store failure on the registry-miss fallback path — are returned
// raw so the caller can map them to Internal.
var ErrShardAuthFailed = errors.New("shard heartbeat authentication failed")

// Admin shard-op preconditions, wrapped by ForceArchive/DeleteArchived so
// the coordinator's gRPC handlers can map them to status codes
// (NotFound / FailedPrecondition) instead of a blanket Internal.
var (
	ErrShardNotInRegistry   = errors.New("shard not found in registry")
	ErrShardWrongState      = errors.New("shard is not in the required state")
	ErrShardWithinRetention = errors.New("shard data is within the retention window")
	ErrShardHasResources    = errors.New("shard still owns an instance or volume")
)

// heartbeatRetention is how long a shard_heartbeats row survives without a
// refresh. Far longer than any reader looks back (adoptFromHeartbeats reads one
// minute), so it only bounds the table; the hour keeps a replaced instance's
// last beat around for debugging.
const heartbeatRetention = time.Hour

// HeartbeatStore is the interface the provisioner needs for heartbeat operations.
type HeartbeatStore interface {
	LiveShardHeartbeats(since string) ([]store.ShardHeartbeat, error)
	RecordHeartbeat(hb *store.ShardHeartbeat) error
	PruneShardHeartbeats(maxAge time.Duration) error
}

// ShardOpsStore is the shard_records read/stamp surface the provisioner
// needs beyond plain persistence — the read-routing service lookups and
// the bucket-service membership reads. (The old LAUNCHING-row lifecycle
// methods are gone; the FSM mints + persists rows directly.) Separate
// from HeartbeatStore so tests that don't exercise this path can keep
// their minimal mocks. *store.Store satisfies it natively.
type ShardOpsStore interface {
	SaveShard(sh *Shard) error
	DeleteShard(id string) error
	GetShard(id string) (*Shard, bool, error)
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
	// VolumeSizesGB maps tier → fresh data-volume size in GiB, parsed from
	// SHARD_<TIER>_VOLUME_GB at boot (logdb.parseShardVolumeSizes). Threaded
	// onto ShardDeps so a fresh launch sizes its EBS volume per tier. Nil in
	// dev/test, where the docker launcher ignores volume size anyway.
	VolumeSizesGB map[string]int32
	// Exports and ExportManifests enable the ephemeral archive export; both
	// nil (dev, tests, or unconfigured prod) keeps the in-process Archive.
	// MaxConcurrentExports caps export instances in flight (0 → default).
	// Threaded onto ShardDeps. See docs/plans/ephemeral-archive-export.md.
	Exports              ExportOps
	ExportManifests      shard.ManifestReader
	ExportOutputs        ObjectDeleter
	MaxConcurrentExports int
	// Holder identifies this process for the "shard-fsm-leader" lease (see
	// docs/plans/coordinator-horizontal-scaling.md #5) — the same holder
	// id main.go's coordinatorHolder() mints for the NAT/poller/cron
	// leases. Start no-ops the election when Holder is empty (or Store is
	// nil): NewProvisioner defaults isLeader() to true, so this process
	// just stays leader unconditionally — the pre-#5 behavior, and the
	// expected shape for every test that constructs a Provisioner directly
	// without going through Start.
	Holder string
	// MaxConcurrentEvaluate is the number of concurrent Evaluate slots:
	// the bucket pass fans out over it per tick, and shard Evaluates
	// share it across ticks as a worker pool (see enqueueShardEvaluations).
	// Zero (the production default) uses maxConcurrentEvaluate. Set to 1
	// for a test wired to a single-connection store (openSMTestStore's
	// tx-scoped fixture panics under concurrent access) to force
	// sequential evaluation.
	MaxConcurrentEvaluate int
}

// defaultCloseRebindGrace is the production default for the rebind
// close-defer window: long enough that typical user queries (<1s) and
// most cron-watch pipelines drain on the old conn, short enough that
// connection leak windows don't accumulate.
const defaultCloseRebindGrace = 5 * time.Second

// DefaultProvisionerConfig returns sensible production defaults.
// IngestingPerBucket isn't here — it lives on each bucket row in
// the DB (migration 020).
//
// PollInterval governs the cadence at which both bucket SM and shard
// SM Evaluate runs. Each Evaluate is cheap (a few indexed SELECTs) and
// AWS side-effects only fire on transitions, not every tick — so this
// can run a lot faster than the original 5-minute value without
// meaningful load. At 1 minute, a kill-and-recover cascade (3 ticks
// + ~2 min staleness window + ~30-60s boot) lands in ~4-7 min
// end-to-end rather than the ~10-17 min the 5-minute interval
// produced. Aligns with the 60-second shard→coordinator heartbeat
// cadence: a missed beat is detectable within a couple of ticks.
func DefaultProvisionerConfig() ProvisionerConfig {
	return ProvisionerConfig{
		PollInterval:   1 * time.Minute,
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
		// A shard already configured as exactly this id is in the desired
		// state — a returning shard after a coordinator restart, not a
		// failure. Keep the handle so the promote gate admits it. Tolerance
		// for shards predating the idempotent-Configure fix
		// (shard/service/server.go), which now returns success for a same-id
		// re-Configure. REMOVE-WHEN no shard older than that fix is in service.
		if alreadyConfiguredAs(err, req.ShardId) {
			return gs, nil
		}
		gs.Close()
		return nil, err
	}
	return gs, nil
}

// alreadyConfiguredAs reports whether err is the shard's "already configured
// as <id>" rejection for this exact shard id — a benign re-Configure of a
// shard that's already in the right identity, which the promote path should
// treat as success rather than an unreachable failure.
func alreadyConfiguredAs(err error, shardID string) bool {
	return err != nil && strings.Contains(err.Error(), "already configured as "+shardID)
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

	// bucketSMs is the bucket half of the FSM control plane
	// (asg-as-api-stateless.md). The shard half has no separate map: each
	// shard IS its own SM (the registry's entries map is the SM set), bound
	// lazily via ensureSM. check() drives both every tick — they own all
	// lifecycle convergence. smMu guards bucketSMs.
	smMu      sync.RWMutex
	bucketSMs map[int64]*BucketSM
	// sharedDeps is the one ShardDeps every shard references by pointer,
	// built once by shardDeps() under depsOnce.
	depsOnce   sync.Once
	sharedDeps *ShardDeps
	// store is the concrete *store.Store behind config.Store/ShardOps,
	// stashed by SetStore. The FSM control plane needs the concrete type
	// (ShardDeps.Store, the bucket SM's *gorm.DB) that the HeartbeatStore
	// / ShardOpsStore interfaces don't expose.
	store *store.Store

	// clock is the time source threaded into every bucket/shard SM so
	// window alignment (CurrentWindow/NextWindow) is deterministic. Real
	// runs use time.Now; tests pin it to a fixed mid-window instant so a
	// multi-tick convergence can't straddle a real window boundary and
	// spuriously mint a next-window shard. Never nil after NewProvisioner.
	clock func() time.Time

	// leading is whether this process currently holds the
	// "shard-fsm-leader" lease — the gate around evaluateStateMachines and
	// promoteOnHeartbeat (see isLeader, docs/plans/
	// coordinator-horizontal-scaling.md #5). Defaults to true: a
	// Provisioner nobody ever calls startLeaderElection on (every test
	// that builds one directly, plus any Start call with no Holder/Store
	// configured) behaves exactly as it did before #5 existed — single
	// process, always convergence-driving. Only startLeaderElection ever
	// flips it based on real lease ownership.
	leading atomic.Bool
	// lastProgress is the unix-nanos timestamp of the last observed
	// convergence progress — stamped inside evaluateStateMachines, once
	// per tick and again after each individual bucket/shard Evaluate call
	// (not just once per tick, so one stuck shard doesn't mask the others
	// still completing). The election loop's deadman's switch reads this:
	// a process that can still renew the lease in Postgres but is stuck
	// inside one Evaluate call must not keep the lease forever. See
	// docs/plans/coordinator-horizontal-scaling.md #5 "Real gap #3".
	lastProgress atomic.Int64

	// Shard Evaluate work queue (see enqueueShardEvaluations). evalMu
	// guards the four fields below it.
	evalMu sync.Mutex
	// evalQueue holds shards waiting for a free slot, FIFO.
	evalQueue []*Shard
	// evalPending is every shard that is queued or running an Evaluate.
	// A shard in this set is never enqueued again, so one shard is never
	// evaluated twice concurrently and a stuck one costs exactly its own
	// slot.
	evalPending map[string]struct{}
	// evalRunning is the number of Evaluates currently holding a slot.
	evalRunning int
	// evalWG tracks running Evaluates so tests can wait for the queue to
	// drain; production never waits on it.
	evalWG sync.WaitGroup
}

// NewProvisioner creates a provisioner.
func NewProvisioner(registry *ShardRegistry, launcher ShardLauncher, config ProvisionerConfig) *Provisioner {
	p := &Provisioner{
		registry:   registry,
		launcher:   launcher,
		config:     config,
		stats:      NewServiceStats(),
		Buckets:    NewBucketManager(nil),
		configurer: grpcConfigurer{shardSecret: config.ShardSecret},
		bucketSMs:  map[int64]*BucketSM{},
		clock:      time.Now,

		evalPending: map[string]struct{}{},
	}
	p.leading.Store(true)
	return p
}

// nowUTC is the provisioner's clock, normalised to UTC. All window
// arithmetic and CreatedAt stamping flows through here so a test can pin
// time by setting p.clock.
func (p *Provisioner) nowUTC() time.Time { return p.clock().UTC() }

// SetStore provides the store for heartbeat and shard-row operations.
// Called after the store is opened (which may happen after the
// provisioner starts). *store.Store satisfies HeartbeatStore directly;
// shard-row ops go through ShardStore (the gorm-over-logdb.Shard
// persister), which embeds *store.Store. This also (re)builds the
// BucketManager against the live store, so callers no longer wire
// buckets in a separate step.
func (p *Provisioner) SetStore(s *store.Store) {
	p.config.Store = s
	p.config.ShardOps = ShardStore{s}
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
// boundary so *Shard never leaks across, and the registry
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
func (p *Provisioner) ShardDescriptorsFor(ctx context.Context, tr TimeRange, namespaceID uint, service string, tier store.Tier) []*pb.ShardDescriptor {
	var serving map[string]struct{}
	if namespaceID != 0 && p.config.ShardOps != nil {
		s, err := p.config.ShardOps.ShardIDsServingService(namespaceID, service)
		if err != nil {
			weftsdk.Infof(ctx, "provisioner: ShardDescriptorsFor: serving lookup ns=%d service=%q: %v — falling back to no filter", namespaceID, service, err)
		} else {
			serving = s
		}
	}
	entries := p.registry.MatchingEntries(tr, serving, tier)
	out := make([]*pb.ShardDescriptor, len(entries))
	for i, e := range entries {
		e.mu.RLock()
		d := &pb.ShardDescriptor{ShardId: e.ID, Services: serviceNames(e.Services)}
		if e.State == StateArchived {
			d.LambdaUrl = p.config.LambdaFunctionURL
			d.ParquetPath = e.ParquetPath
		} else {
			d.GrpcAddr = e.Addr
		}
		e.mu.RUnlock()
		out[i] = d
	}
	return out
}

// Start runs the provisioner loop until the context is cancelled.
// It ensures at least one INGESTING shard exists on startup.
func (p *Provisioner) Start(ctx context.Context) {
	p.startLeaderElection(ctx)
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

	readAt := p.clock()
	heartbeats, err := p.config.Store.LiveShardHeartbeats("1 minute")
	if err != nil {
		weftsdk.Errorf(context.Background(), "provisioner: heartbeat query failed: %v", err)
		return
	}
	// The table keeps one row per source, so expired rows (replaced
	// instances, christened ones, gone shards) are the only thing to trim.
	// Idempotent and a handful of rows, so every coordinator may run it.
	if err := p.config.Store.PruneShardHeartbeats(heartbeatRetention); err != nil {
		weftsdk.Infof(context.Background(), "provisioner: prune shard_heartbeats: %v", err)
	}

	for _, hb := range heartbeats {
		p.ProcessHeartbeat(hb)
	}
	// After the beats are applied, so no reader pairs the new read time with
	// a not-yet-refreshed beat. A failed read leaves it where it was: not
	// knowing is not evidence a shard died.
	p.registry.MarkLivenessRead(readAt)
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
		// HeartbeatToken isn't immutable — RefreshFromStore rewrites it
		// under e.mu every tick — so read it under the entry lock rather
		// than racing that write (auth runs on the heartbeat RPC goroutine,
		// the refresh on the reconciler tick).
		entry.mu.RLock()
		expectedToken = entry.HeartbeatToken
		entry.mu.RUnlock()
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
		e := p.shardByInstance(hb.InstanceID)
		if e == nil {
			// No LAUNCHING shard is awaiting this instance — a stray
			// unclaimed heartbeat (orphan instance, or one already
			// promoted). The stale-LAUNCHING path reaps genuine orphans.
			return
		}
		p.promoteOnHeartbeat(ctx, e, hb)
		return
	}

	e := p.registry.Get(hb.ShardID)
	if e == nil {
		// Unknown shard id — a shard we have no record of, nothing to drive.
		return
	}
	e.ensureSM(p.shardDeps())

	e.mu.RLock()
	state := e.State
	e.mu.RUnlock()
	switch state {
	case StateLaunching, StateSuspended:
		p.promoteOnHeartbeat(ctx, e, hb)
	case StateUpgrading:
		// Operator-driven roll: the FSM has decided to reap this instance.
		// Heartbeats from the still-running process must NOT promote
		// (UPGRADING has no Heartbeat permit by design — see configureSM)
		// and must NOT ObserveHeartbeat (its reconnect path would undo
		// onEnterUpgrading's teardown and race the planned reap). Drop
		// silently; evaluateRecovery will reap on the next tick.
	default:
		e.ObserveHeartbeat(ctx, hb)
	}
}

// promoteOnHeartbeat gates a LAUNCHING/SUSPENDED shard's promotion on a
// successful Configure dial: the coordinator must be able to reach the shard
// (coordinator → shard:9000) before it joins the ingest cohort. Otherwise an
// instance that heartbeats fine but is unreachable — e.g. a new-AZ
// security-group gap — would promote to INGESTING with a nil Live handle,
// get silently skipped by the ingest router (which requires Live != nil),
// and strand a Target slot doing nothing while heartbeats keep it "fresh".
//
// On a dial failure we log and leave the shard where it is; the next
// heartbeat retries, so it self-heals once the network is fixed without any
// relaunch churn (a LAUNCHING shard counts as in-flight, so the bucket SM
// holds rather than minting a replacement into the same wall).
//
// Gated behind isLeader(): firing an FSM transition is exclusive, same as
// evaluateStateMachines. A non-leader coordinator that happens to receive
// this heartbeat's RPC (heartbeats aren't sticky-routed to the leader)
// silently drops it here — the persisted row from Server.Heartbeat's
// RecordHeartbeat is unaffected, so the leader's own adoptFromHeartbeats
// catch-up sweep (unconditional, every tick) picks it up within one
// PollInterval. See docs/plans/coordinator-horizontal-scaling.md #5.
func (p *Provisioner) promoteOnHeartbeat(ctx context.Context, e *Shard, hb store.ShardHeartbeat) {
	if !p.isLeader() {
		return
	}
	dialed, err := e.dialConfigure(ctx, hb)
	if err != nil {
		weftsdk.Infof(ctx, "provisioner: heartbeat %s (instance %s): not promoting — coordinator can't reach %s: %v", e.ID, hb.InstanceID, hb.Addr, err)
		return
	}
	if err := e.Fire(ctx, TriggerHeartbeat, hb, dialed); err != nil {
		weftsdk.Infof(ctx, "provisioner: heartbeat promote %s: %v", e.ID, err)
	}
	// If the guard rejected the promotion (e.g. a stray-instance heartbeat),
	// configureOnHeartbeat never ran and the handle wasn't attached — close
	// it so we don't leak a connection.
	if dialed != nil {
		e.mu.RLock()
		attached := e.Live == dialed
		e.mu.RUnlock()
		if !attached {
			dialed.Close()
		}
	}
}

// shardByInstance finds the LAUNCHING shard whose entry carries the given
// instance id — the correlation an empty-ShardID heartbeat needs to reach
// the shard the bucket minted for this instance. Only LAUNCHING entries
// are candidates: a promoted shard heartbeats with its real id and routes
// by id instead. The returned shard has its SM bound, ready to Fire.
func (p *Provisioner) shardByInstance(instanceID string) *Shard {
	if instanceID == "" {
		return nil
	}
	for _, e := range p.registry.Entries() {
		e.mu.RLock()
		match := e.State == StateLaunching && e.InstanceID == instanceID
		e.mu.RUnlock()
		if match {
			e.ensureSM(p.shardDeps())
			return e
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
func (p *Provisioner) OnboardService(namespaceID uint, service string) error {
	if namespaceID == 0 || service == "" {
		return nil
	}
	// Canonical mapping: record (namespace, service) as a log_group in the
	// default bucket so shards minted at the next christen inherit it
	// (StampShardServices reads log_groups). Idempotent — a service already
	// onboarded via weftd/cloudwatch keeps its source type.
	if p.store != nil {
		if err := p.store.EnsureServiceLogGroup(namespaceID, service); err != nil {
			return fmt.Errorf("OnboardService(%d/%s): ensure log_group: %w", namespaceID, service, err)
		}
	}
	// Immediate: stamp the default bucket's INGESTING shards from its
	// log_groups (now including this service) so ingest + query routing
	// pick it up from the next row rather than waiting for the next drift
	// sweep. A service lands in the default bucket (EnsureServiceLogGroup
	// leaves bucket_id NULL); its shards are that bucket's shards.
	if p.Buckets == nil {
		return nil
	}
	bucket, err := p.Buckets.Default()
	if err != nil {
		return fmt.Errorf("OnboardService(%d/%s): lookup default bucket: %w", namespaceID, service, err)
	}
	if bucket != nil {
		p.syncServicesForBucket(bucket)
	}
	return nil
}

// check runs one tick of FSM-driven convergence: refresh the registry
// from the store, sweep service-drift, then drive every bucket and shard
// SM toward its target. The SMs own all lifecycle transitions — launch,
// rotate, archive, recover — so check() no longer emits reconciler ops or
// syncs ASG desired_capacity (the EC2Provisioner runs/terminates one
// instance at a time inside Launch/Terminate). Bucket-split (carving a hot
// service
// into its own bucket) is disabled pending the redesign in
// docs/plans/bucket-split-policy.md.
func (p *Provisioner) check(ctx context.Context) {
	p.registry.RefreshFromStore(!p.isLeader())

	// Service-drift: a service onboarded after a shard christened isn't on
	// its stamp yet. The FSM cascade is deliberately lifecycle-only, so
	// this membership sweep (the old reconciler's op 1) stays a standalone
	// pass rather than a bucket-SM concern. Unconditional — AddServiceToShard
	// is idempotent, so an already-present membership is a cheap no-op even
	// run by a coordinator that isn't the shard-fsm-leader. See
	// docs/plans/coordinator-horizontal-scaling.md #5.
	p.syncShardServices()

	// FSM convergence — buckets first (cascade directives + minting), then
	// shards (their own launch/stale/recovery/archive work). Exclusive:
	// launches, terminates, and persisted state transitions must only ever
	// be driven by one coordinator at a time, so this is gated behind the
	// shard-fsm-leader lease. See docs/plans/coordinator-horizontal-scaling.md
	// #5.
	if p.isLeader() {
		p.evaluateStateMachines(ctx)
	}
}

// isLeader reports whether this process currently holds the
// "shard-fsm-leader" lease. See the leading field comment and
// docs/plans/coordinator-horizontal-scaling.md #5.
func (p *Provisioner) isLeader() bool { return p.leading.Load() }

// markProgress stamps lastProgress with the current time — called once per
// evaluateStateMachines tick (via defer, so an idle tick with nothing to
// converge still counts as progress) and again after each individual
// bucket/shard Evaluate call, so a single stuck shard doesn't mask the
// others still completing. See the lastProgress field comment.
func (p *Provisioner) markProgress() { p.lastProgress.Store(time.Now().UnixNano()) }

// maxConcurrentEvaluate is the default number of concurrent Evaluate slots.
// Sequential evaluation made one slow shard's Evaluate (e.g. a stuck AWS
// call during archive or recovery) block every other shard's convergence —
// confirmed under real prod load in docs/known-issues.md
// "evaluateStateMachines runs shards sequentially". Bounded (not
// unbounded) so a large fleet doesn't fan out into hundreds of
// simultaneous DB/AWS calls.
const maxConcurrentEvaluate = 8

// evaluateConcurrently runs eval(item) for every item in items, with at
// most limit running at once, and blocks until all have finished. Pulled
// out of evaluateStateMachines so the bounded-fan-out behavior — not tied
// to Shard or BucketSM — is directly unit-testable
// (TestEvaluateStateMachines_ShardsConvergeConcurrently exercises it
// through the real tick; the property itself needs no shard machinery).
func evaluateConcurrently[T any](items []T, limit int, eval func(T)) {
	if limit <= 0 || limit > len(items) {
		limit = len(items)
	}
	if limit == 0 {
		return
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(item T) {
			defer wg.Done()
			defer func() { <-sem }()
			eval(item)
		}(item)
	}
	wg.Wait()
}

// evaluateStateMachines drives every bucket SM then every shard SM one
// tick toward convergence. Buckets run first: a WARMING bucket mints new
// shard SMs, and re-snapshotting after the bucket pass lets those minted
// shards Evaluate the same tick (their launch starts now, not next tick).
// The bucket pass is waited on: bucket Evaluates are DB-only (FireShard
// just fires an FSM trigger; side effects live in the shard's own
// Evaluate), and finishing it first is what lets a freshly minted shard
// be enqueued in the same tick. The shard pass is not waited on — shards
// are handed to a worker pool (enqueueShardEvaluations) so a slow or
// stuck Evaluate (archive export, hung AWS call) costs only its own
// slot and never delays the tick, heartbeat adoption, or other shards.
//
// Only ever called while holding the shard-fsm-leader lease (see check) —
// callers driving this directly in tests are implicitly leader via
// NewProvisioner's leading default.
func (p *Provisioner) evaluateStateMachines(ctx context.Context) {
	defer p.markProgress()
	tickStart := time.Now()
	p.ensureBucketSMs()

	p.smMu.RLock()
	buckets := make([]*BucketSM, 0, len(p.bucketSMs))
	for _, b := range p.bucketSMs {
		buckets = append(buckets, b)
	}
	p.smMu.RUnlock()
	limit := p.config.MaxConcurrentEvaluate
	if limit <= 0 {
		limit = maxConcurrentEvaluate
	}
	evaluateConcurrently(buckets, limit, func(b *BucketSM) {
		start := time.Now()
		err := b.Evaluate(ctx)
		store.LogEvaluateEvent("bucket", fmt.Sprint(b.ID), time.Since(start), err)
		if err != nil {
			weftsdk.Infof(ctx, "provisioner: bucket %d Evaluate: %v", b.ID, err)
		}
		p.markProgress()
	})

	shards := p.registry.Entries()
	p.enqueueShardEvaluations(ctx, shards)
	store.LogEvaluateTick(len(buckets), len(shards), time.Since(tickStart))
}

// enqueueShardEvaluations hands shards to the Evaluate worker pool and
// returns without waiting for any of them. maxConcurrentEvaluate (or
// config.MaxConcurrentEvaluate) is the number of slots; a slot frees the
// moment its Evaluate returns and immediately starts the next queued
// shard, so a fleet bigger than the slot count still drains in one pass
// rather than one batch per tick. A shard already queued or running is
// skipped — it keeps its place (or its slot) instead of being evaluated
// twice concurrently.
func (p *Provisioner) enqueueShardEvaluations(ctx context.Context, shards []*Shard) {
	p.evalMu.Lock()
	defer p.evalMu.Unlock()
	for _, e := range shards {
		if _, busy := p.evalPending[e.ID]; busy {
			continue
		}
		p.evalPending[e.ID] = struct{}{}
		p.evalQueue = append(p.evalQueue, e)
	}
	p.startQueuedEvaluationsLocked(ctx)
}

// startQueuedEvaluationsLocked starts queued shards while slots are free.
// Caller holds evalMu.
func (p *Provisioner) startQueuedEvaluationsLocked(ctx context.Context) {
	limit := p.config.MaxConcurrentEvaluate
	if limit <= 0 {
		limit = maxConcurrentEvaluate
	}
	for p.evalRunning < limit && len(p.evalQueue) > 0 {
		e := p.evalQueue[0]
		p.evalQueue = p.evalQueue[1:]
		p.evalRunning++
		p.evalWG.Add(1)
		go p.runShardEvaluate(ctx, e)
	}
}

// runShardEvaluate runs one shard's Evaluate in a slot, then frees the
// slot and starts the next queued shard.
func (p *Provisioner) runShardEvaluate(ctx context.Context, e *Shard) {
	defer p.evalWG.Done()
	// A shard queued under a lease this process has since lost must not be
	// driven: the new leader owns convergence.
	if p.isLeader() {
		e.ensureSM(p.shardDeps())
		start := time.Now()
		err := e.Evaluate(ctx)
		store.LogEvaluateEvent("shard", e.ID, time.Since(start), err)
		if err != nil {
			weftsdk.Infof(ctx, "provisioner: shard %s Evaluate: %v", e.ID, err)
		}
		p.markProgress()
	}

	p.evalMu.Lock()
	defer p.evalMu.Unlock()
	delete(p.evalPending, e.ID)
	p.evalRunning--
	p.startQueuedEvaluationsLocked(ctx)
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
		weftsdk.Infof(context.Background(), "provisioner: ensureBucketSMs: Buckets.All: %v", err)
		return
	}
	p.smMu.Lock()
	defer p.smMu.Unlock()
	for _, bk := range all {
		if sm, ok := p.bucketSMs[bk.ID]; ok {
			sm.Bucket = bk
		} else {
			p.bucketSMs[bk.ID] = NewBucketSM(bk, p.store.DB(), p, p.clock)
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
		weftsdk.Infof(context.Background(), "provisioner: syncShardServices: Buckets.All: %v", err)
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
		weftsdk.Infof(context.Background(), "provisioner: services for bucket %d: %v", bucket.ID, err)
		return
	}
	for _, e := range p.registry.Entries() {
		e.mu.RLock()
		skip := e.BucketID != bucket.ID || e.State != StateIngesting
		e.mu.RUnlock()
		if skip {
			continue
		}
		for _, s := range svcs {
			p.registry.AddServiceToShard(e.ID, s.NamespaceID, s.Service)
		}
	}
}

// ForceArchive is the admin-facing entry point for archiving a READ_ONLY
// shard on demand, bypassing the bucket's retention-window sweep. It drives
// the shard FSM rather than a parallel synchronous path: READ_ONLY →
// ARCHIVING (ArchiveRequested), then one Evaluate pass runs the shard's own
// export → retire-volume → terminate → ArchiveComplete sequence to the
// ARCHIVED terminal. evaluateArchiving is idempotent and step-gated, so a
// transient failure leaves the shard ARCHIVING for the reconciler tick to
// retry — surfaced here as a non-nil error.
func (p *Provisioner) ForceArchive(shardID string) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s: %w", shardID, ErrShardNotInRegistry)
	}
	entry.ensureSM(p.shardDeps())
	entry.mu.RLock()
	st := entry.State
	entry.mu.RUnlock()
	if st != StateReadOnly {
		return fmt.Errorf("shard %s is %s, not READ_ONLY: %w", shardID, st, ErrShardWrongState)
	}
	ctx := context.Background()
	if err := entry.Fire(ctx, TriggerArchiveRequested); err != nil {
		return fmt.Errorf("force-archive %s: %w", shardID, err)
	}
	if err := entry.Evaluate(ctx); err != nil {
		return fmt.Errorf("force-archive %s: %w", shardID, err)
	}
	entry.mu.RLock()
	st = entry.State
	entry.mu.RUnlock()
	if st != StateArchived {
		return fmt.Errorf("force-archive %s: did not complete (state %s)", shardID, st)
	}
	return nil
}

// DeleteArchived removes an archived shard by driving the shard FSM's only
// edge out of ARCHIVED: AdminDelete → GONE. onEnterGone removes the entry
// from the registry (closing its handle) and deletes the shard_records
// row. The Parquet data on S3 is left in place (tombstone cleanup is a
// separate concern).
//
// Unless force is set, a shard whose window still falls within its tier's
// retention is refused with ErrShardWithinRetention — the gate that used
// to live in the API admin handler.
func (p *Provisioner) DeleteArchived(shardID string, force bool) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s: %w", shardID, ErrShardNotInRegistry)
	}
	entry.ensureSM(p.shardDeps())
	entry.mu.RLock()
	st := entry.State
	windowEnd := entry.WindowEnd
	bucketID := entry.BucketID
	entry.mu.RUnlock()
	if st != StateArchived {
		return fmt.Errorf("shard %s is %s, not ARCHIVED: %w", shardID, st, ErrShardWrongState)
	}
	if !force && p.store != nil {
		if b, found, _ := p.store.GetBucket(bucketID); found {
			retention := store.LogRetention(store.Tier(b.Tier))
			if time.Since(windowEnd) < retention {
				return fmt.Errorf("shard %s is within the %s retention window: %w", shardID, retention, ErrShardWithinRetention)
			}
		}
	}
	if err := entry.Fire(context.Background(), TriggerAdminDelete); err != nil {
		return fmt.Errorf("delete archived %s: %w", shardID, err)
	}
	weftsdk.Infof(context.Background(), "provisioner: archived shard %s deleted", shardID)
	return nil
}

// ForceRemove discards a shard stuck in ARCHIVING by firing AdminDelete
// (ARCHIVING → GONE); onEnterGone drops the registry entry and deletes the
// shard_records row. It exists for archives that can never complete: the
// host and volume are gone and no export was recorded, so evaluateArchiving
// loops forever (ArchiveComplete needs a parquet path).
//
// Unless force is set, a shard that still records an instance or volume is
// refused with ErrShardHasResources — removing the row would orphan them
// (no FSM would ever retire them). Any S3 data is left in place.
func (p *Provisioner) ForceRemove(shardID string, force bool) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s: %w", shardID, ErrShardNotInRegistry)
	}
	entry.ensureSM(p.shardDeps())
	entry.mu.RLock()
	st := entry.State
	instanceID := entry.InstanceID
	volumeID := entry.EBSVolumeID
	entry.mu.RUnlock()
	if st != StateArchiving {
		return fmt.Errorf("shard %s is %s, not ARCHIVING: %w", shardID, st, ErrShardWrongState)
	}
	if !force && (instanceID != "" || volumeID != "") {
		return fmt.Errorf("shard %s (instance %q, volume %q): %w", shardID, instanceID, volumeID, ErrShardHasResources)
	}
	if err := entry.Fire(context.Background(), TriggerAdminDelete); err != nil {
		return fmt.Errorf("force-remove %s: %w", shardID, err)
	}
	weftsdk.Infof(context.Background(), "provisioner: stuck ARCHIVING shard %s force-removed (instance=%q volume=%q force=%v)", shardID, instanceID, volumeID, force)
	return nil
}

// RelaunchShard is the admin-facing entry point for an in-place binary
// upgrade. Drives the shard FSM through TriggerRelaunchRequested →
// UPGRADING, where the shared reap-and-relaunch recovery branch takes
// over (evaluateRecovery): the still-running instance is terminated, a
// fresh instance is launched from the ASG's current launch template (=
// current AMI), and the surviving EBS volume is reattached in the same
// AZ. The shard reconstructs its identity from shard.json on the
// reattached volume, heartbeats, and the promote path lifts it back to
// INGESTING — same shard id, same window, same data, new binary.
//
// UPGRADING is a distinct state from SUSPENDED (which HeartbeatStale
// uses) because the operator-roll target is a HEALTHY shard whose
// process keeps heartbeating; SUSPENDED's transient-blip heartbeat
// permit would silently no-op the roll. UPGRADING omits that permit.
//
// Only "live with data" states are valid (INGESTING / READ_ONLY /
// DEGRADED). LAUNCHING is already coming up; ARCHIVING/ARCHIVED/GONE/
// SUSPENDED/UPGRADING have nothing to upgrade and are rejected with
// ErrShardWrongState so the caller can map it to FailedPrecondition.
func (p *Provisioner) RelaunchShard(shardID string) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s: %w", shardID, ErrShardNotInRegistry)
	}
	entry.ensureSM(p.shardDeps())
	entry.mu.RLock()
	st := entry.State
	entry.mu.RUnlock()
	if st != StateIngesting && st != StateReadOnly && st != StateDegraded {
		return fmt.Errorf("shard %s is %s, cannot relaunch (only INGESTING/READ_ONLY/DEGRADED): %w", shardID, st, ErrShardWrongState)
	}
	if err := entry.Fire(context.Background(), TriggerRelaunchRequested); err != nil {
		return fmt.Errorf("relaunch %s: %w", shardID, err)
	}
	weftsdk.Infof(context.Background(), "provisioner: relaunch requested for %s (was %s); driving via UPGRADING → LAUNCHING", shardID, st)
	return nil
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
