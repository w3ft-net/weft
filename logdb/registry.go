package logdb

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qmuntal/stateless"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// ShardState and the State* constants live in states.go now —
// see also docs/plans/asg-as-api-stateless.md for the full FSM.

// Shard is the unified shard type: it IS the persisted row (gorm-tagged,
// table shard_records), the in-memory runtime object the registry routes
// against, and the thing the shard FSM mutates. Collapsing the old
// store.ShardRecord + logdb.ShardEntry pair into one type deletes the
// hand-written Record↔Entry mapping that drift bugs lived in (see
// docs/plans/shard-type-collapse.md).
//
// Field groups: persisted columns carry gorm tags; runtime-only fields
// carry `gorm:"-"`. The unexported mu is ignored by gorm automatically.
//
// Concurrency: an entry is shared between the registry (which reads its
// fields to route queries/writes) and the shard SM (which mutates them on
// transitions, launches, and heartbeats). mu guards every MUTABLE field
// below — both sides take it. The registry's own r.mu guards the entries
// MAP; mu guards an entry's FIELDS. Lock order when both are held is
// r.mu → mu; the SM only ever holds mu (and must not call a registry
// method that takes r.mu while holding it). ID is immutable after
// construction and safe to read lock-free.
type Shard struct {
	mu sync.RWMutex `gorm:"-"`

	// ── persisted (maps to table shard_records) ──
	ID              string     `gorm:"primaryKey"`
	WindowStart     time.Time  `gorm:"not null"` // timestamptz (migration 062)
	WindowEnd       time.Time  `gorm:"not null"` // timestamptz (migration 062)
	State           ShardState `gorm:"not null"`
	IngestFailures  int        `gorm:"not null;default:0;column:ingest_failures"` // consecutive ingest failures; resets on success
	Addr            string     // gRPC address for live shards
	InstanceID      string     // EC2 instance ID
	ParquetPath     string     // Parquet file path for archived shards
	Tier            string     // instance tier (small/medium/large)
	RowCount        int64      `gorm:"default:0"`
	CreatedAt       time.Time  `gorm:"not null"` // timestamptz (migration 062)
	EBSVolumeID     string     // EBS volume id for this shard's DuckDB; empty until volume is attached
	HeartbeatToken  string     // per-shard bearer the Heartbeat handler validates against
	BucketID        int64      `gorm:"not null;column:bucket_id"`                 // surrogate FK to buckets.id (migration 022)
	LastHeartbeatAt time.Time  `gorm:"column:last_heartbeat_at"`                  // wall-clock of the most recent observed heartbeat; zero = never (treated as fresh)
	AZ              string     `gorm:"column:az"`                                 // FSM striping: pins recovery launches to the volume's AZ
	StripeIndex     *int       `gorm:"column:stripe_index"`                       // bucket-assigned stripe position; nil = unassigned (distinct from 0)
	LaunchAttempts  int        `gorm:"not null;default:0;column:launch_attempts"` // per-shard launch retry counter

	// ── runtime only (gorm:"-") ──
	// Services is the shard's in-memory service membership — the
	// (namespace_id, service) pairs it serves, the write router's
	// counterpart to the read router's shard_services lookup. Stamped at
	// christen from the bucket's log_groups and appended on live
	// service-add; loaded from the shard_services table at boot/refresh.
	// An empty list means the shard serves nothing (unreachable for any
	// specific-service query or write).
	Services []store.ShardService `gorm:"-" json:"-"`
	// Live is the live connection handle (gRPC for hot shards, ColdShard
	// for archived). Not persisted; rehydrated lazily by ReconstructShard
	// at boot and by the heartbeat-reconnect path. json:"-" so the admin
	// list-shards handler (which JSON-encodes []Shard) never tries to
	// marshal a live connection.
	Live shard.Shard `gorm:"-" json:"-"`

	// ── FSM machinery (gorm:"-", not persisted) ──
	// The shard IS its own finite state machine: sm is the stateless
	// engine whose external storage reads/writes shard_records.state (see
	// readState/writeState in shardsm.go); deps is the shared collaborator
	// bundle the provisioner owns (one pointer, every shard references it).
	// Both are bound by bindSM once the entry enters the registry — a
	// gorm-loaded row has no SM until the provisioner binds it. See
	// docs/plans/shard-type-collapse.md Phase 2.
	sm   *stateless.StateMachine `gorm:"-" json:"-"`
	deps *ShardDeps              `gorm:"-" json:"-"`
	// launchInFlight is set while an async launch goroutine is working so
	// successive Evaluate ticks don't kick a second launch for the same
	// shard. In-memory only: a restart resets it and the next Evaluate
	// re-drives any LAUNCHING row that never got an InstanceID.
	launchInFlight atomic.Bool `gorm:"-" json:"-"`
}

// TableName pins the unified Shard type to the existing shard_records
// table — the DB schema is unchanged by the type collapse.
func (*Shard) TableName() string { return "shard_records" }

// SuspendFailureThreshold is the number of consecutive ingest failures
// that marks a shard as suspended. One flaky RPC shouldn't take a shard
// out of rotation; a sustained run of failures should.
const SuspendFailureThreshold = 3

// heartbeatStaleThreshold is how long since the last heartbeat before
// MatchingShards/IngestingShard treat an entry as dead and skip it.
// Aligns with adoptFromHeartbeats's "2 minutes" lookback window: a
// shard that hasn't heartbeat'd in that window won't be re-adopted
// after a coordinator restart either, so the registry shouldn't keep
// routing traffic to it.
const heartbeatStaleThreshold = 2 * time.Minute

// isStale reports whether this entry's last observed heartbeat is
// older than heartbeatStaleThreshold. Zero (never set) counts as
// fresh — see the LastHeartbeatAt field comment. Caller must hold
// e.mu (read).
func (e *Shard) isStale() bool {
	if e.LastHeartbeatAt.IsZero() {
		return false
	}
	return time.Since(e.LastHeartbeatAt) > heartbeatStaleThreshold
}

// serves reports whether this entry serves the given (namespaceID,
// service) — the write router's membership test against the shard's
// in-memory service set. An empty service matches any service in the
// namespace (the namespace-wide check), mirroring the read path's
// ShardIDsServingService("" = namespace-only) semantics. Caller must
// hold e.mu (read).
func (e *Shard) serves(namespaceID uint, service string) bool {
	for _, s := range e.Services {
		if s.NamespaceID == namespaceID && (service == "" || s.Service == service) {
			return true
		}
	}
	return false
}

// RegistryStore is the persistence surface ShardRegistry needs. The
// shard-row methods take/return the unified *Shard (no Record↔Entry
// mapping); production satisfies it with ShardStore (gorm over
// shard_records), tests with an in-memory fake.
type RegistryStore interface {
	SaveShard(sh *Shard) error
	DeleteShard(id string) error
	ListShards() ([]*Shard, error)
	GetShard(id string) (*Shard, bool, error)
	TouchHeartbeat(id string, rowCount int64) error
	// ListShardServices returns every shard_services row; the registry
	// groups it by shard_id to populate each entry's in-memory service
	// membership at boot/refresh.
	ListShardServices() ([]store.ShardService, error)
	// AddShardService records that a shard serves (namespaceID, service)
	// — the live-add persistence behind AddServiceToShard.
	AddShardService(shardID string, namespaceID uint, service string) error
}

// ShardRegistry tracks all shard entries. Thread-safe.
// If a Store is set, mutations are persisted.
type ShardRegistry struct {
	mu      sync.RWMutex
	entries map[string]*Shard
	robin   atomic.Uint64 // round-robin counter for write distribution
	store   RegistryStore // optional persistence (nil in tests)
}

// NewShardRegistry creates an empty registry.
func NewShardRegistry() *ShardRegistry {
	return &ShardRegistry{
		entries: make(map[string]*Shard),
	}
}

// SetStore enables persistence. Accepts *store.Store in production and
// an in-memory RegistryStore in tests.
func (r *ShardRegistry) SetStore(s RegistryStore) {
	r.store = s
}

// Add registers a shard entry and persists it.
func (r *ShardRegistry) Add(entry *Shard) {
	r.mu.Lock()
	r.entries[entry.ID] = entry
	r.mu.Unlock()
	r.persist(entry)
}

// persist saves a shard entry to the store (if configured).
// BucketID is required (NOT NULL FK). No silent fallback — callers
// must set it explicitly before Add/PersistState; a zero value
// surfaces as a DB constraint error, which we log so silent
// persistence failures don't slip past tests or prod.
func (r *ShardRegistry) persist(entry *Shard) {
	if r.store == nil {
		return
	}
	if err := r.store.SaveShard(entry.persistedCopy()); err != nil {
		log.Printf("registry: persist shard %s: %v", entry.ID, err)
	}
}

// persistedCopy returns a fresh *Shard holding a snapshot of e's
// persisted columns, taken under e.mu. The registry hands this copy to
// the store rather than the live entry the SM concurrently mutates, so
// gorm's reflection over the struct can't race a field write. The copy's
// runtime fields (Services/Live) and mutex are left zero — gorm ignores
// them (gorm:"-" / unexported). Unlike the old persist this snapshots
// every persisted column (AZ, stripe, launch_attempts, last_heartbeat_at,
// ingest_failures included), so a full Save no longer wipes the columns
// the narrow writes used to have to restore.
func (e *Shard) persistedCopy() *Shard {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return &Shard{
		ID:              e.ID,
		WindowStart:     e.WindowStart,
		WindowEnd:       e.WindowEnd,
		State:           e.State,
		IngestFailures:  e.IngestFailures,
		Addr:            e.Addr,
		InstanceID:      e.InstanceID,
		ParquetPath:     e.ParquetPath,
		Tier:            e.Tier,
		RowCount:        e.RowCount,
		CreatedAt:       e.CreatedAt,
		EBSVolumeID:     e.EBSVolumeID,
		HeartbeatToken:  e.HeartbeatToken,
		BucketID:        e.BucketID,
		LastHeartbeatAt: e.LastHeartbeatAt,
		AZ:              e.AZ,
		StripeIndex:     e.StripeIndex,
		LaunchAttempts:  e.LaunchAttempts,
	}
}

// PersistState saves the current state of an entry to the store.
// Call after modifying State, ParquetPath, RowCount, etc.
func (r *ShardRegistry) PersistState(entry *Shard) {
	r.persist(entry)
}

// TouchHeartbeat persists only heartbeat-owned fields (row_count)
// without overwriting lifecycle-owned fields like window bounds,
// state, or apps. Prevents the heartbeat path from clobbering
// manual DB edits or reconciler-driven state changes.
func (r *ShardRegistry) TouchHeartbeat(entry *Shard) {
	if r.store == nil {
		return
	}
	entry.mu.RLock()
	rowCount := entry.RowCount
	entry.mu.RUnlock()
	if err := r.store.TouchHeartbeat(entry.ID, rowCount); err != nil {
		log.Printf("registry: touch heartbeat %s: %v", entry.ID, err)
	}
}

// RefreshFromStore re-reads every shard record from the DB and
// updates in-memory metadata fields to match. The Shard pointer
// (live gRPC connection) is preserved — only declarative fields
// (state, window bounds, bucket_id, apps, etc.) are overwritten.
// This closes the drift window between the DB and the registry:
// manual edits, out-of-band state changes, or PersistState calls
// from the heartbeat path that clobber a field changed underneath
// all get corrected within one tick.
func (r *ShardRegistry) RefreshFromStore() {
	if r.store == nil {
		return
	}
	fresh, err := r.store.ListShards()
	if err != nil {
		log.Printf("registry: RefreshFromStore: %v", err)
		return
	}
	svcByShard, svcOK := r.servicesByShard()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range fresh {
		entry, ok := r.entries[f.ID]
		if !ok {
			continue
		}
		entry.mu.Lock()
		entry.State = f.State
		entry.WindowStart = f.WindowStart
		entry.WindowEnd = f.WindowEnd
		entry.CreatedAt = f.CreatedAt
		entry.Addr = f.Addr
		entry.InstanceID = f.InstanceID
		entry.ParquetPath = f.ParquetPath
		entry.Tier = f.Tier
		entry.RowCount = f.RowCount
		entry.EBSVolumeID = f.EBSVolumeID
		entry.HeartbeatToken = f.HeartbeatToken
		entry.BucketID = f.BucketID
		// Liveness fields the in-memory entry owns more freshly than the
		// row (LastHeartbeatAt, IngestFailures) and FSM-launch bookkeeping
		// (AZ, StripeIndex, LaunchAttempts) are deliberately NOT refreshed
		// here — matching the pre-collapse RefreshFromStore.
		// Only refresh services when the shard_services read succeeded;
		// on a transient error leave the existing membership rather than
		// wiping live routing to nil.
		if svcOK {
			entry.Services = svcByShard[f.ID]
		}
		entry.mu.Unlock()
	}
}

// servicesByShard loads every shard_services row and groups it by
// shard_id, the form LoadFromStore/RefreshFromStore use to (re)populate
// each entry's in-memory service membership. The bool is false on a
// store error (logged) so callers can leave existing memberships intact
// rather than clobbering live routing.
func (r *ShardRegistry) servicesByShard() (map[string][]store.ShardService, bool) {
	out := map[string][]store.ShardService{}
	if r.store == nil {
		return out, true
	}
	rows, err := r.store.ListShardServices()
	if err != nil {
		log.Printf("registry: ListShardServices: %v", err)
		return out, false
	}
	for _, s := range rows {
		out[s.ShardID] = append(out[s.ShardID], s)
	}
	return out, true
}

// LoadFromStore loads every shard record from the store into the
// registry. The reconstruct function attaches a live shard.Shard
// (typically a ColdShard for ARCHIVED rows) when it can; if it
// returns an error the entry still loads, just without a Shard
// pointer — the missing Shard gets attached lazily by the
// heartbeat-adopt path or an explicit dial when the row's
// instance reappears.
//
// Pre-fix this dropped any record whose reconstruct failed —
// every non-ARCHIVED row, because cmd/coordinator/main.go's
// reconstruct only handled cold shards. That left every live row
// invisible to the registry after a coordinator restart, breaking
// every code path that walked Entries (the reconciler's Op 3
// rotate-at-WindowEnd in particular). See
// coordinator/zombie_repro_test.go for the cascade.
func (r *ShardRegistry) LoadFromStore(reconstruct func(sh *Shard) (shard.Shard, error)) error {
	if r.store == nil {
		return nil
	}
	shards, err := r.store.ListShards()
	if err != nil {
		return err
	}
	svcByShard, _ := r.servicesByShard()
	for _, sh := range shards {
		// The loaded *Shard IS the registry entry — no Record↔Entry
		// mapping. reconstruct's error is informational: a non-ARCHIVED
		// row (or one we can't open data for yet) still belongs in the
		// registry, just with a nil Live handle the heartbeat path
		// reattaches later.
		live, _ := reconstruct(sh)
		sh.Services = svcByShard[sh.ID]
		sh.Live = live

		r.mu.Lock()
		r.entries[sh.ID] = sh
		r.mu.Unlock()
	}
	return nil
}

// Remove deletes a shard entry and closes its connection.
func (r *ShardRegistry) Remove(id string) {
	r.mu.Lock()
	if e, ok := r.entries[id]; ok {
		e.mu.Lock()
		if e.Live != nil {
			e.Live.Close()
		}
		e.mu.Unlock()
		delete(r.entries, id)
	}
	r.mu.Unlock()

	if r.store != nil {
		r.store.DeleteShard(id)
	}
}

// AddServiceToShard appends (namespaceID, service) to the given shard's
// in-memory service set and persists it to shard_services, under the
// registry's write lock. Idempotent (no-op if already present) and
// state-gated (only INGESTING shards grow their membership — READ_ONLY/
// ARCHIVED shards' services stay frozen, since they never accepted
// writes for a service onboarded after they sealed).
//
// Returns (found, added):
//   - found=false means the shard isn't in the registry
//   - found=true, added=false means the op was a no-op (already
//     present or state != INGESTING)
//   - found=true, added=true means the shard's service set grew
//
// Callers that mutate entry.Services directly bypass the lock and race
// with IngestingShard/MatchingEntries readers — don't.
func (r *ShardRegistry) AddServiceToShard(id string, namespaceID uint, service string) (found bool, added bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[id]
	if !ok {
		return false, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.State != StateIngesting {
		return true, false
	}
	if entry.serves(namespaceID, service) {
		return true, false
	}
	entry.Services = append(entry.Services, store.ShardService{
		ShardID: id, NamespaceID: namespaceID, Service: service,
	})
	if r.store != nil {
		if err := r.store.AddShardService(id, namespaceID, service); err != nil {
			log.Printf("registry: AddServiceToShard persist %s (%d/%s): %v", id, namespaceID, service, err)
		}
	}
	return true, true
}

// RecordIngestFailure increments the consecutive-failure counter for the
// given shard and returns (count, threshold) so Shard.Ingest impls can
// format an error message naming how persistent the failure is. It no
// longer latches the shard out of write routing — a truly dead shard is
// taken out by the FSM (HeartbeatStale → SUSPENDED), and a persistently
// unhealthy-but-alive shard is future work (a persisted IngestUnhealthy
// column). Pair with ResetIngestFailures on success so transient blips
// don't accumulate.
func (r *ShardRegistry) RecordIngestFailure(id string) (count, threshold int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return 0, SuspendFailureThreshold
	}
	e.IngestFailures++
	return e.IngestFailures, SuspendFailureThreshold
}

// ResetIngestFailures clears the consecutive-failure counter for a shard
// after a successful ingest.
func (r *ShardRegistry) ResetIngestFailures(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.IngestFailures = 0
	}
}

// Get returns a shard entry by ID, or nil.
func (r *ShardRegistry) Get(id string) *Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[id]
}

// HasAddr reports whether any live entry is registered at addr.
// Used by the christen path to skip a duplicate Configure when two
// heartbeats from the same unclaimed instance race through
// ProcessHeartbeat — the second one's captured ShardID is stale
// (empty) because the shard's id is only written after the first
// Configure returns, so without this check the second goroutine
// would mint a new id and try to re-Configure the same addr.
func (r *ShardRegistry) HasAddr(addr string) bool {
	if addr == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		e.mu.RLock()
		match := e.Addr == addr
		e.mu.RUnlock()
		if match {
			return true
		}
	}
	return false
}

// MatchingEntries returns shards whose time windows overlap the given
// range AND whose ID is in the serving set. serving is the set of shard
// IDs that serve the queried (namespace, service), resolved upstream
// from shard_services (see Store.ShardIDsServingService). A nil serving
// set means "no service filter" — every shard with an overlapping
// window matches (the namespace-blind, all-services fan-out a bare
// /log/warp.log or an unresolved namespace produces). An empty (non-nil)
// set matches nothing: the query named a service no shard serves.
//
// MatchingEntries stays pure — it does no DB I/O. The serving-set lookup
// is the caller's job (ShardDescriptorsFor), keeping the registry free
// of store knowledge and the routing tests DB-free.
//
// tr.Start is clamped up to now − LogRetention(tier) so a customer
// never reaches a shard whose data falls entirely outside their
// advertised retention. Boundary-spanning shards (window started
// before the cap, ended after it) still match — the planner's
// `ts >= TIMESTAMP '...'` predicate (built from the same already-
// clamped tr at the call site) hides the portion before the cap
// within that shard. The clamp here is defense in depth; passing the
// already-clamped value keeps it idempotent. tier="" yields the
// trial-tier retention (24h, conservative).
func (r *ShardRegistry) MatchingEntries(tr TimeRange, serving map[string]struct{}, tier store.Tier) []*Shard {
	if cap := time.Now().Add(-store.LogRetention(tier)); tr.Start.Before(cap) {
		tr.Start = cap
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*Shard
	for _, e := range r.entries {
		if e.queryable(tr, serving) {
			out = append(out, e)
		}
	}
	return out
}

// queryable applies MatchingEntries' per-entry filter under the entry's
// read lock. Note: the entry pointer it qualifies is still returned to
// the caller, which must take e.mu before reading its fields (e.g.
// ShardDescriptorsFor does).
func (e *Shard) queryable(tr TimeRange, serving map[string]struct{}) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.Addr == "" && e.Live == nil {
		return false
	}
	// SUSPENDED (instance dead, Addr/InstanceID cleared, awaiting rebind)
	// is unqueryable: it keeps a stale non-nil Shard pointer so the
	// Addr==""/Shard==nil guard doesn't catch it, and routing it only
	// yields "no grpc_addr or lambda_url".
	if e.State == StateSuspended {
		return false
	}
	if e.isStale() {
		return false
	}
	// An ARCHIVED / ARCHIVING / READ_ONLY shard with zero rows has nothing
	// to return; dialing it is pure waste. ARCHIVING is grouped with
	// READ_ONLY (its query-routing "substate" relationship): it stays
	// queryable until the export swaps in the ColdShard. Live INGESTING
	// shards are exempt: a transient 0 count just means it hasn't filled.
	if e.RowCount == 0 && (e.State == StateArchived || e.State == StateArchiving || e.State == StateReadOnly) {
		return false
	}
	hasWindow := !e.WindowStart.IsZero() && !e.WindowEnd.IsZero()
	if hasWindow && (!e.WindowStart.Before(tr.End) || !e.WindowEnd.After(tr.Start)) {
		return false
	}
	if serving != nil {
		if _, ok := serving[e.ID]; !ok {
			return false
		}
	}
	return true
}

// IngestingShard returns an INGESTING shard serving (namespaceID,
// service), selected via round-robin across all matching INGESTING
// shards. This distributes writes across N concurrent writable shards
// for the same service.
func (r *ShardRegistry) IngestingShard(namespaceID uint, service string) shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var candidates []shard.Shard
	for _, e := range r.entries {
		if sh := e.ingestCandidate(namespaceID, service); sh != nil {
			candidates = append(candidates, sh)
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	idx := r.robin.Add(1) % uint64(len(candidates))
	return candidates[idx]
}

// ingestCandidate returns the live shard handle if this entry is an
// INGESTING, fresh, serving shard, else nil. Takes the entry read lock.
func (e *Shard) ingestCandidate(namespaceID uint, service string) shard.Shard {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.State != StateIngesting || e.Live == nil || e.isStale() {
		return nil
	}
	if e.serves(namespaceID, service) {
		return e.Live
	}
	return nil
}

// TotalIngesting returns the count of all INGESTING shards regardless of app.
func (r *ShardRegistry) TotalIngesting() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.entries {
		e.mu.RLock()
		ingesting := e.State == StateIngesting && e.Live != nil
		e.mu.RUnlock()
		if ingesting {
			n++
		}
	}
	return n
}

// IngestingCount returns the number of INGESTING shards serving
// (namespaceID, service).
func (r *ShardRegistry) IngestingCount(namespaceID uint, service string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.entries {
		e.mu.RLock()
		ok := e.State == StateIngesting && e.Live != nil && e.serves(namespaceID, service)
		e.mu.RUnlock()
		if ok {
			n++
		}
	}
	return n
}

// Entries returns a snapshot of all entries (for health/status reporting).
func (r *ShardRegistry) Entries() []*Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Shard, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

// Count returns the number of registered shards.
func (r *ShardRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
