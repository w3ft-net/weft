package logdb

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"github.com/w3ft-net/weft/backends/shardstore"

	"github.com/qmuntal/stateless"

	"github.com/w3ft-net/weft/shard"
)

// ShardState and the State* constants live in states.go now.

// Shard is the unified shard type: the in-memory runtime object the
// registry routes against, and the thing the shard FSM mutates. It embeds
// ShardRow, the tag-free core of what is persisted; the store converts
// to and from its own record type at its boundary (shard_record.go), so no
// ORM model is part of the contract. The hosted-only facts (export,
// upgrade, last purge) sit beside the row.
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
	mu sync.RWMutex

	// facts is what the shard has reported about its field values; see
	// fieldfacts.go. Guarded by its own lock, not mu.
	facts fieldFactSet

	// ShardRow is the persisted row; its fields are promoted onto the entry.
	// The hosted-only facts in it (Export, Upgrade) are zero in
	// the open build. Export.Attempt is monotonic and survives clearing the
	// rest of the export facts. The facts are written through the store's
	// narrow methods and mirrored onto the entry, because SaveShard upserts the
	// whole row and would otherwise write a stale copy back; RefreshFromStore
	// takes the store's view. Upgrade.Origin is the state an upgrade returns
	// to, and what write routing treats the shard as meanwhile.
	ShardRow

	// ── runtime only ──
	// Services is the shard's in-memory service membership — the
	// (namespace_id, service) pairs it serves, the write router's
	// counterpart to the read router's shard_services lookup. Stamped at
	// christen from the bucket's log_groups and appended on live
	// service-add; loaded from the shard_services table at boot/refresh.
	// An empty list means the shard serves nothing (unreachable for any
	// specific-service query or write).
	Services []shardstore.ShardService `json:"-"`
	// Live is the live connection handle (gRPC for hot shards, ColdShard
	// for archived). Not persisted; rehydrated lazily by ReconstructShard
	// at boot and by the heartbeat-reconnect path. json:"-" so the admin
	// list-shards handler (which JSON-encodes []Shard) never tries to
	// marshal a live connection.
	Live shard.Shard `json:"-"`

	// ── FSM machinery (not persisted) ──
	// The shard IS its own finite state machine: sm is the stateless
	// engine whose external storage reads/writes shard_records.state (see
	// readState/writeState in shardsm.go); deps is the shared collaborator
	// bundle the provisioner owns (one pointer, every shard references it).
	// Both are bound by bindSM once the entry enters the registry — a
	// gorm-loaded row has no SM until the provisioner binds it.
	sm   *stateless.StateMachine `json:"-"`
	deps *ShardDeps              `json:"-"`
	// launchInFlight is set while an async launch goroutine is working so
	// successive Evaluate ticks don't kick a second launch for the same
	// shard. In-memory only: a restart resets it and the next Evaluate
	// re-drives any LAUNCHING row that never got an InstanceID.
	launchInFlight atomic.Bool `json:"-"`
	// degradedAt is when this shard entered DEGRADED — the start of the
	// ingest circuit-breaker cooldown evaluateDegraded measures against.
	// In-memory only (guarded by mu): a restart loses it, so a DEGRADED
	// shard re-admits immediately after a restart (optimistic — it retries
	// writes and re-degrades if still broken).
	degradedAt time.Time `json:"-"`
	// reconnectFailures counts consecutive coordinator→shard dial failures
	// on the heartbeat reconnect path (ObserveHeartbeat). A shard that
	// heartbeats fine but the coordinator can't dial back (a new-AZ
	// security-group gap, a partition) would otherwise sit INGESTING with a
	// nil Live handle forever — fresh, but silently skipped by the ingest
	// router. Crossing SuspendFailureThreshold trips it to DEGRADED via the
	// same breaker IngestFailures uses. Reset on a successful reconnect and
	// on DEGRADED re-admit. In-memory (guarded by mu).
	reconnectFailures int `json:"-"`

	// locEpoch counts in-process moves of the shard's location (Addr,
	// InstanceID, Live). RefreshFromStore snapshots it before reading rows and
	// skips the location fields of any entry whose epoch moved since: its row
	// read predates the move. Guarded by mu.
	locEpoch uint64 `json:"-"`
}

// ingestRecoveryCooldown is how long a shard stays DEGRADED before
// Evaluate re-admits it to INGESTING (half-open). Matches the heartbeat
// stale window: long enough that a flapping shard isn't re-admitted every
// tick, short enough that a transient ingest blip clears quickly.
const ingestRecoveryCooldown = 2 * time.Minute

// SuspendFailureThreshold is the number of consecutive ingest failures
// that marks a shard as suspended. One flaky RPC shouldn't take a shard
// out of rotation; a sustained run of failures should.
const SuspendFailureThreshold = 3

// heartbeatStaleThreshold is how long since the last heartbeat before
// MatchingShards/IngestingShard treat an entry as dead and skip it.
// Aligns with adoptFromHeartbeats's "1 minute" lookback window: a
// shard that hasn't heartbeat'd in that window won't be re-adopted
// after a coordinator restart either, so the registry shouldn't keep
// routing traffic to it.
//
// At a 10s shard→coordinator heartbeat cadence, 1 minute = 6 missed
// beats — high confidence the shard is gone, with low false-positive
// risk from transient network blips (SUSPENDED's heartbeat-rescue
// permit still resurrects healthy shards whose beats hiccup briefly).
const heartbeatStaleThreshold = 1 * time.Minute

// isStale reports whether this entry's last observed heartbeat was
// older than heartbeatStaleThreshold as of asOf, the last moment this
// coordinator could have seen a newer beat (see LivenessAsOf). Zero (never
// set) counts as fresh — see the LastHeartbeatAt field comment. Caller must
// hold e.mu (read).
func (e *Shard) isStale(asOf time.Time) bool {
	if e.LastHeartbeatAt.IsZero() {
		return false
	}
	return asOf.Sub(e.LastHeartbeatAt) > heartbeatStaleThreshold
}

// MarkLivenessRead records that a read of every shard's latest heartbeat
// (the shard_heartbeats sweep) started at at and has been applied to the
// entries. Only moves forward.
func (r *ShardRegistry) MarkLivenessRead(at time.Time) {
	n := at.UnixNano()
	for {
		cur := r.livenessReadAt.Load()
		if n <= cur || r.livenessReadAt.CompareAndSwap(cur, n) {
			return
		}
	}
}

// LivenessAsOf is the moment this coordinator's heartbeat knowledge is
// current as of: the start of the last applied heartbeat sweep, or now when
// no sweep has run (no store wired). A shard's heartbeat stream is pinned to
// one coordinator, so every other coordinator learns of its beats only from
// the sweep; judging their copy against the wall clock would let a late
// sweep (a slow FSM tick, a GC pause, a DB hiccup) drop healthy shards. A
// shard is stale only once a read shows it went quiet, so the sweep cadence
// sets how fast a dead shard is noticed, never whether a live one is
// routed. The coordinator holding the stream stamps each beat as it lands,
// later than any sweep, so it stays fresh between sweeps too.
func (r *ShardRegistry) LivenessAsOf() time.Time {
	if n := r.livenessReadAt.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Now()
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

// repointable is a live handle whose address can move under it
// (shard.GRPCShard).
type repointable interface {
	Addr() string
	Home() string
	Repoint(addr string)
}

// liveAddr is the address e's live handle actually talks to, or "" when it
// has none (or can't say). Caller holds e.mu.
func (e *Shard) liveAddr() string {
	if m, ok := e.Live.(repointable); ok {
		return m.Addr()
	}
	return ""
}

// RegistryStore is the persistence surface ShardRegistry needs. The
// shard-row methods take/return the unified *Shard (no Record↔Entry
// mapping); production satisfies it with PGShardStore (gorm over
// shard_records), tests with an in-memory fake.
type RegistryStore interface {
	SaveShard(sh *ShardRow) error
	DeleteShard(id string) error
	ListShards() ([]*ShardRow, error)
	GetShard(id string) (*ShardRow, bool, error)
	TouchHeartbeat(id string, rowCount int64, at time.Time, binarySHA string, diskBytes int64) error
	// SetShardIngestFailures persists the consecutive-ingest-failure
	// counter. Narrow write, mirroring TouchHeartbeat — see
	// RecordIngestFailure/ResetIngestFailures.
	SetShardIngestFailures(id string, n int) error
	// ListShardServices returns every shard_services row; the registry
	// groups it by shard_id to populate each entry's in-memory service
	// membership at boot/refresh.
	ListShardServices() ([]shardstore.ShardService, error)
	// AddShardService records that a shard serves (namespaceID, service)
	// — the live-add persistence behind AddServiceToShard.
	AddShardService(shardID string, namespaceID uint, service string) error
	// ListShardFieldFacts returns every shard's typed field facts, loaded
	// at boot and on refresh like ListShardServices.
	ListShardFieldFacts() ([]shardstore.ShardFieldFact, error)
	// ApplyShardFieldFacts records one heartbeat's changes to a shard's
	// field facts: typed facts upserted, dropped ones deleted.
	ApplyShardFieldFacts(shardID string, typed, dropped []shardstore.ShardFieldFact) error
}

// SetLegacyFactTable installs the resolver for field facts that name a source
// by customer. Set it before LoadFromStore, which reads the store's facts.
func (r *ShardRegistry) SetLegacyFactTable(f LegacyFactTable) { r.legacyFactTable = f }

// ShardRegistry tracks all shard entries. Thread-safe.
// If a Store is set, mutations are persisted.
type ShardRegistry struct {
	mu      sync.RWMutex
	entries map[string]*Shard
	robin   atomic.Uint64 // round-robin counter for write distribution
	// livenessReadAt is MarkLivenessRead's time in unix nanos; 0 = never.
	livenessReadAt atomic.Int64
	store          RegistryStore // optional persistence (nil in tests)
	// legacyFactTable resolves the field facts of a source known only by
	// customer; see LegacyFactTable. Nil: such facts are unknown.
	legacyFactTable LegacyFactTable
	// reconstruct is LoadFromStore's live-handle builder, kept so
	// RefreshFromStore attaches a shard it adopts the same way boot does.
	reconstruct func(sh *Shard) (shard.Shard, error)
	// now is the clock write routing judges a shard's window against. Nil: the
	// wall clock.
	now func() time.Time
}

// RoutingSkew is how far two clocks (a coordinator's, and the writer's that
// stamped a row) may disagree about which window an instant falls in. A query
// reads a shard whose window ends or starts within it of the query's range, so a
// row that landed on the neighbouring window's shard by that much is still found.
const RoutingSkew = 5 * time.Second

// clock is the registry's current time.
func (r *ShardRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
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
		weftsdk.Infof(context.Background(), "registry: persist shard %s: %v", entry.ID, err)
	}
}

// persistedCopy returns a snapshot of e's persisted row, taken under e.mu.
// The registry hands this copy to the store rather than the live entry the SM
// concurrently mutates, so a store's reflection over the struct can't race a
// field write. It snapshots every persisted column, so a full save no longer
// wipes the columns the narrow writes used to have to restore.
// entryOf is a new entry, with no runtime state bound, for a row read from a
// store. It owns its own copy of the row.
func entryOf(row *ShardRow) *Shard { return &Shard{ShardRow: row.Clone()} }

// entriesOf is entryOf of each row.
func entriesOf(rows []*ShardRow) []*Shard {
	out := make([]*Shard, len(rows))
	for i, r := range rows {
		out[i] = entryOf(r)
	}
	return out
}

func (e *Shard) persistedCopy() *ShardRow {
	e.mu.RLock()
	defer e.mu.RUnlock()
	row := e.ShardRow.Clone()
	return &row
}

// copyTimePtr returns an independent copy of *t, so a persistedCopy never
// aliases the live entry's pointer.
func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// PersistState saves the current state of an entry to the store.
// Call after modifying State, ParquetPath, RowCount, etc.
func (r *ShardRegistry) PersistState(entry *Shard) {
	r.persist(entry)
}

// TouchHeartbeat persists only the heartbeat-owned fields (row_count,
// disk_bytes, last_heartbeat_at, and binary_sha) without overwriting lifecycle-owned
// fields like window bounds, state, or apps. Prevents the heartbeat path
// from clobbering manual DB edits or reconciler-driven state changes.
func (r *ShardRegistry) TouchHeartbeat(entry *Shard) {
	if r.store == nil {
		return
	}
	entry.mu.RLock()
	rowCount := entry.RowCount
	at := entry.LastHeartbeatAt
	sha := entry.BinarySHA
	disk := entry.DiskBytes
	entry.mu.RUnlock()
	if err := r.store.TouchHeartbeat(entry.ID, rowCount, at, sha, disk); err != nil {
		weftsdk.Infof(context.Background(), "registry: touch heartbeat %s: %v", entry.ID, err)
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
//
// A row the registry doesn't hold is adopted, loaded as LoadFromStore
// would: only the shard-fsm-leader mints, so every other coordinator (and
// a newly-elected leader) learns of a shard minted after its boot here.
// Without it a follower never routes to a new cohort and DLQs every row
// once the old one rotates out (the 2026-10-18 outage).
//
// dropDeleted removes entries whose row is gone. Only non-leaders pass it:
// the leader is the one writer that deletes rows and removes its own entry
// as it does (onEnterGone), and pruning there would race MintShard, whose
// entry lands in the map before its row is written.
func (r *ShardRegistry) RefreshFromStore(dropDeleted bool) {
	if r.store == nil {
		return
	}
	epochs := r.locationEpochs()
	rows, err := r.store.ListShards()
	if err != nil {
		weftsdk.Infof(context.Background(), "registry: RefreshFromStore: %v", err)
		return
	}
	fresh := entriesOf(rows)
	svcByShard, svcOK := r.servicesByShard()
	factsByShard, factsOK := r.fieldFactsByShard()
	r.mu.Lock()
	defer r.mu.Unlock()
	if dropDeleted {
		inStore := make(map[string]bool, len(fresh))
		for _, f := range fresh {
			inStore[f.ID] = true
		}
		for id, e := range r.entries {
			if inStore[id] {
				continue
			}
			e.mu.Lock()
			if e.Live != nil {
				e.Live.Close()
			}
			e.mu.Unlock()
			delete(r.entries, id)
			weftsdk.Infof(context.Background(), "registry: dropped shard %s (its record was deleted)", id)
		}
	}
	for _, f := range fresh {
		entry, ok := r.entries[f.ID]
		if !ok {
			if svcOK {
				f.Services = svcByShard[f.ID]
			}
			if factsOK {
				f.replaceFieldFacts(r.legacyFactTable, factsByShard[f.ID])
			}
			if r.reconstruct != nil {
				f.Live, _ = r.reconstruct(f)
			}
			r.entries[f.ID] = f
			weftsdk.Infof(context.Background(), "registry: adopted shard %s (%s) from store", f.ID, f.State)
			continue
		}
		entry.mu.Lock()
		entry.State = f.State
		entry.WindowStart = f.WindowStart
		entry.WindowEnd = f.WindowEnd
		entry.CreatedAt = f.CreatedAt
		// The location fields move together, and only if nothing in this process
		// moved the shard since the rows were read: a swap that completed
		// meanwhile left f holding the old instance.
		if entry.locEpoch == epochs[f.ID] {
			entry.Addr = f.Addr
			// The row's addr is the only way a coordinator that doesn't hold the
			// shard's heartbeat stream learns the shard moved to another instance
			// (an upgrade's volume swap). Move the live handle with it, or this
			// coordinator keeps writing to the old instance.
			if m, ok := entry.Live.(repointable); ok && f.Addr != "" && m.Home() != f.Addr {
				m.Repoint(f.Addr)
				weftsdk.Infof(context.Background(), "registry: shard %s moved to %s; repointed its handle", f.ID, f.Addr)
			}
			entry.InstanceID = f.InstanceID
		}
		entry.ParquetPath = f.ParquetPath
		entry.Tier = f.Tier
		entry.RowCount = f.RowCount
		entry.DiskBytes = f.DiskBytes
		entry.EBSVolumeID = f.EBSVolumeID
		entry.HeartbeatToken = f.HeartbeatToken
		entry.BucketID = f.BucketID
		// FSM-launch bookkeeping (AZ, StripeIndex, LaunchAttempts) and
		// IngestFailures are refreshed from the DB like everything else
		// above. The DB is never behind memory for
		// AZ/StripeIndex/LaunchAttempts (both writers persist before
		// mutating locally, shardsm.go:481-498); IngestFailures now
		// persists synchronously in RecordIngestFailure/ResetIngestFailures
		// below, so it's the same shape. Only LastHeartbeatAt keeps the old
		// "in-memory owns it more freshly" exception — every coordinator
		// bumps its own copy off the same shard_heartbeats rows regardless
		// of leadership (see the plan doc for why gating that would be a
		// trap), so refreshing it here would make a coordinator that isn't
		// currently processing this shard's heartbeats fight itself.
		entry.AZ = f.AZ
		entry.StripeIndex = f.StripeIndex
		entry.LaunchAttempts = f.LaunchAttempts
		entry.IngestFailures = f.IngestFailures
		// Export facts: like AZ/StripeIndex, the DB is never behind memory
		// (every writer persists before mutating the entry), and after a
		// leader handover the new leader's entry must see the old leader's
		// in-flight attempt rather than start a second one.
		entry.Export = f.Export
		// Upgrade facts: same — and a follower needs UpgradeOrigin to keep
		// write-routing an INGESTING shard through its upgrade.
		entry.Upgrade = f.Upgrade
		// Only refresh services when the shard_services read succeeded;
		// on a transient error leave the existing membership rather than
		// wiping live routing to nil.
		if svcOK {
			entry.Services = svcByShard[f.ID]
		}
		entry.mu.Unlock()
		// The store is never behind memory for facts (a heartbeat persists
		// before it acknowledges), so a follower takes the store's view.
		if factsOK {
			entry.replaceFieldFacts(r.legacyFactTable, factsByShard[f.ID])
		}
	}
}

// servicesByShard loads every shard_services row and groups it by
// shard_id, the form LoadFromStore/RefreshFromStore use to (re)populate
// each entry's in-memory service membership. The bool is false on a
// store error (logged) so callers can leave existing memberships intact
// rather than clobbering live routing.
func (r *ShardRegistry) servicesByShard() (map[string][]shardstore.ShardService, bool) {
	out := map[string][]shardstore.ShardService{}
	if r.store == nil {
		return out, true
	}
	rows, err := r.store.ListShardServices()
	if err != nil {
		weftsdk.Infof(context.Background(), "registry: ListShardServices: %v", err)
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
	r.reconstruct = reconstruct
	if r.store == nil {
		return nil
	}
	rows, err := r.store.ListShards()
	if err != nil {
		return err
	}
	shards := entriesOf(rows)
	svcByShard, _ := r.servicesByShard()
	factsByShard, factsOK := r.fieldFactsByShard()
	for _, sh := range shards {
		// The loaded *Shard IS the registry entry — no Record↔Entry
		// mapping. reconstruct's error is informational: a non-ARCHIVED
		// row (or one we can't open data for yet) still belongs in the
		// registry, just with a nil Live handle the heartbeat path
		// reattaches later.
		live, _ := reconstruct(sh)
		sh.Services = svcByShard[sh.ID]
		if factsOK {
			sh.replaceFieldFacts(r.legacyFactTable, factsByShard[sh.ID])
		}
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

// RefreshArchivedPath repoints an archived entry at a rewritten parquet
// after the janitor swapped its file in Postgres. Query descriptors read
// the entry's in-memory ParquetPath, so without this new queries would
// keep hitting the old object — which the GC sweep deletes after the
// grace window, turning later reads into 404s. Builds the replacement
// ColdShard before taking the lock (NewColdShard may open an in-memory
// DuckDB in the no-Lambda path), then swaps ParquetPath + Live under the
// entry lock. No-op if the entry is gone (already retired).
func (r *ShardRegistry) RefreshArchivedPath(id, newPath, lambdaURL string) {
	cold, err := shard.NewColdShard(id, nil, newPath, lambdaURL, nil)
	if err != nil {
		weftsdk.Infof(context.Background(), "registry: refresh %s cold shard: %v", id, err)
	}
	r.mu.RLock()
	e := r.entries[id]
	r.mu.RUnlock()
	if e == nil {
		if cold != nil {
			cold.Close()
		}
		return
	}
	e.mu.Lock()
	e.ParquetPath = newPath
	if e.Live != nil {
		e.Live.Close()
	}
	if cold != nil {
		e.Live = cold
	} else {
		e.Live = nil
	}
	e.mu.Unlock()
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
	if !entry.writable() {
		return true, false
	}
	if entry.serves(namespaceID, service) {
		return true, false
	}
	entry.Services = append(entry.Services, shardstore.ShardService{
		ShardID: id, NamespaceID: namespaceID, Service: service,
	})
	if r.store != nil {
		if err := r.store.AddShardService(id, namespaceID, service); err != nil {
			weftsdk.Infof(context.Background(), "registry: AddServiceToShard persist %s (%d/%s): %v", id, namespaceID, service, err)
		}
	}
	return true, true
}

// RecordIngestFailure increments the consecutive-failure counter for the
// given shard and returns (count, threshold) so Shard.Ingest impls can
// format an error message naming how persistent the failure is. It
// doesn't itself change routing — the next reconciler tick's
// evaluateIngesting reads this count and, once it crosses
// SuspendFailureThreshold, fires IngestDegraded (INGESTING → DEGRADED),
// the FSM-native circuit breaker that replaced the old one-way Suspended
// latch. Pair with ResetIngestFailures on success so transient blips
// don't accumulate.
func (r *ShardRegistry) RecordIngestFailure(id string) (count, threshold int) {
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return 0, SuspendFailureThreshold
	}
	e.IngestFailures++
	count = e.IngestFailures
	r.mu.Unlock()
	// Persisted synchronously (mirrors TouchHeartbeat) so a leader gated
	// behind #5's shard-fsm-leader lease reads a counter this process — or
	// whichever coordinator is actually routing this shard's ingest writes
	// — is actually incrementing, rather than the leader's own local (and
	// possibly always-zero) copy. The
	// failure path is not the hot path, so an unconditional write here
	// isn't a throughput concern the way it would be on every reset below.
	if r.store != nil {
		if err := r.store.SetShardIngestFailures(id, count); err != nil {
			weftsdk.Infof(context.Background(), "registry: persist ingest failures %s: %v", id, err)
		}
	}
	return count, SuspendFailureThreshold
}

// ResetIngestFailures clears the consecutive-failure counter for a shard
// after a successful ingest. Only writes through to the store when the
// counter actually had something to clear — Ingest.Ingest calls this on
// every successful batch (the hot path), so persisting an already-zero
// counter on every single call would be pure write amplification under
// load.
func (r *ShardRegistry) ResetIngestFailures(id string) {
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	hadFailures := e.IngestFailures != 0
	e.IngestFailures = 0
	r.mu.Unlock()
	if hadFailures && r.store != nil {
		if err := r.store.SetShardIngestFailures(id, 0); err != nil {
			weftsdk.Infof(context.Background(), "registry: persist ingest failures reset %s: %v", id, err)
		}
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
// tr.Start is clamped up to earliest, the earliest instant the caller may
// read, so a customer never reaches a shard whose data falls entirely
// outside their advertised retention. Boundary-spanning shards (window
// started before the cap, ended after it) still match — the planner's
// `ts >= TIMESTAMP '...'` predicate (built from the same already-
// clamped tr at the call site) hides the portion before the cap
// within that shard. The clamp here is defense in depth; passing the
// already-clamped value keeps it idempotent. The zero time means no
// limit: what a deployment without retention passes.
//
// A shard that holds rows for the query but can't be queried (its instance
// is dead or being replaced, or it has stopped heartbeating) is not left
// out: the query has no answer without its rows, so MatchingEntries returns
// an *UnavailableShardsError naming every such shard instead.
func (r *ShardRegistry) MatchingEntries(tr TimeRange, serving map[string]struct{}, earliest time.Time) ([]*Shard, error) {
	if tr.Start.Before(earliest) {
		tr.Start = earliest
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	asOf := r.LivenessAsOf()
	var out []*Shard
	var down []UnavailableShard
	for _, e := range r.entries {
		holds, why := e.queryable(tr, serving, asOf)
		switch {
		case !holds:
		case why != "":
			down = append(down, UnavailableShard{ID: e.ID, Why: why})
		default:
			out = append(out, e)
		}
	}
	if len(down) > 0 {
		sort.Slice(down, func(i, j int) bool { return down[i].ID < down[j].ID })
		return nil, &UnavailableShardsError{Shards: down}
	}
	return out, nil
}

// UnavailableShard is a shard a query needs that can't be queried, and why.
type UnavailableShard struct {
	ID  string
	Why string
}

// UnavailableShardsError fails a query some of whose rows sit on shards
// that can't be queried. An answer from the remaining shards would be a
// wrong answer that looks right, so there is none.
type UnavailableShardsError struct {
	Shards []UnavailableShard
}

func (e *UnavailableShardsError) Error() string {
	parts := make([]string, len(e.Shards))
	for i, d := range e.Shards {
		parts[i] = d.ID + " (" + d.Why + ")"
	}
	return "can't answer without " + strings.Join(parts, ", ") + ": they hold rows in this query's time range"
}

// queryable applies MatchingEntries' per-entry filter under the entry's
// read lock. Note: the entry pointer it qualifies is still returned to
// the caller, which must take e.mu before reading its fields (e.g.
// ShardDescriptorsFor does).
//
// It reports whether the entry holds rows this query needs (holds) and,
// when it does, why it can't be queried (why; "" = queryable).
func (e *Shard) queryable(tr TimeRange, serving map[string]struct{}, asOf time.Time) (holds bool, why string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	// GONE's data is destroyed: it has no rows anything could return. A
	// fresh LAUNCHING shard has never ingested; a relaunch after SUSPENDED
	// or UPGRADING reattaches the old volume and keeps its row count.
	if e.State == StateGone || (e.State == StateLaunching && e.RowCount == 0) {
		return false, ""
	}
	// An ARCHIVED / ARCHIVING / READ_ONLY shard with zero rows has nothing
	// to return; dialing it is pure waste. ARCHIVING is grouped with
	// READ_ONLY (its query-routing "substate" relationship): it stays
	// queryable until the export swaps in the ColdShard. Live INGESTING
	// shards are exempt: a transient 0 count just means it hasn't filled.
	if e.RowCount == 0 && (e.State == StateArchived || e.State == StateArchiving || e.State == StateReadOnly) {
		return false, ""
	}
	// The window is widened by RoutingSkew each side: a row stamped within a
	// moment of a boundary may sit on the neighbouring window's shard.
	hasWindow := !e.WindowStart.IsZero() && !e.WindowEnd.IsZero()
	if hasWindow && (!e.WindowStart.Add(-RoutingSkew).Before(tr.End) || !e.WindowEnd.Add(RoutingSkew).After(tr.Start)) {
		return false, ""
	}
	if serving != nil {
		if _, ok := serving[e.ID]; !ok {
			return false, ""
		}
	}

	// SUSPENDED (instance dead, awaiting rebind) keeps a stale non-nil
	// Shard pointer, so it is caught by state; routing it only yields "no
	// grpc_addr or lambda_url". UPGRADING is a serving state when it moves
	// the volume through the warm pool, and the reap-and-relaunch outage
	// otherwise.
	switch e.State {
	case StateSuspended:
		return true, "SUSPENDED: its instance died"
	case StateUpgrading:
		if e.Upgrade.Phase == "" {
			// Reap-and-relaunch (no warm pool): there is no instance to ask.
			return true, "UPGRADING: its instance is being replaced"
		}
		if e.Upgrade.Phase.VolumeMoving() {
			// The old instance stopped heartbeating when it paused; calls
			// routed here are held by its gate and the client until the
			// successor opens (or the hold times out and the query fails).
			return true, ""
		}
	case StateLaunching:
		return true, "LAUNCHING: its replacement instance is starting"
	}
	// Staleness (missed heartbeats) is a live-instance liveness signal —
	// meaningless once a shard is ARCHIVED, since it has no instance left
	// to go stale. evaluateArchiving clears LastHeartbeatAt in memory when
	// the archive completes, but that clear is never persisted (Store.
	// SetShardParquetPath only updates parquet_path), so LoadFromStore
	// rehydrates the shard's stale pre-archival heartbeat from Postgres on
	// every coordinator restart, and judging it here would fail every
	// query over archived data the moment the coordinator next restarted.
	if e.State != StateArchived && e.isStale(asOf) {
		return true, fmt.Sprintf("no heartbeat for %s", asOf.Sub(e.LastHeartbeatAt).Round(time.Second))
	}
	if e.Addr == "" && e.Live == nil {
		return true, string(e.State) + " with no address to query"
	}
	return true, ""
}

// IngestingShard returns an INGESTING shard serving (namespaceID,
// service), selected via round-robin across all matching INGESTING
// shards. This distributes writes across N concurrent writable shards
// for the same service.
func (r *ShardRegistry) IngestingShard(namespaceID uint, service string) shard.Shard {
	return r.IngestingShardExcept(namespaceID, service, "")
}

// IngestingShardExcept is IngestingShard without the shard exceptID: the one
// a write just failed on, which can still be a candidate (it takes a few
// consecutive failures to drop out), so a retry that simply asks again can
// land on it. Nil when nothing else takes the service.
func (r *ShardRegistry) IngestingShardExcept(namespaceID uint, service, exceptID string) shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	asOf, now := r.LivenessAsOf(), r.clock()
	var serving, upgrading []ingestChoice
	for id, e := range r.entries {
		if id == exceptID {
			continue
		}
		c, isUpgrading := e.ingestCandidate(namespaceID, service, asOf, now)
		switch {
		case c.sh == nil:
		case isUpgrading:
			upgrading = append(upgrading, c)
		default:
			serving = append(serving, c)
		}
	}

	// A shard mid-upgrade takes writes only when nothing else can: with a
	// sibling ingesting, the sibling absorbs them and the paused shard's
	// senders never stall waiting for its successor to open.
	candidates := serving
	if len(candidates) == 0 {
		candidates = upgrading
	}
	if len(candidates) == 0 {
		return nil
	}

	// A write lands where a query for its instant looks: reads route by window,
	// and a row stored on a shard whose window has not opened is invisible to
	// them. The next window's cohort is INGESTING (launched, configured and
	// serving) a PrewarmLead early so that it can take every write the moment its
	// window opens, not so that it takes writes before then. When no candidate's
	// window holds the clock (a rotation that is late, a successor that never
	// came) the writes keep landing on all of them rather than going to the
	// dead-letter queue.
	var open []ingestChoice
	for _, c := range candidates {
		if c.windowOpen {
			open = append(open, c)
		}
	}
	if len(open) > 0 {
		candidates = open
	}
	idx := r.robin.Add(1) % uint64(len(candidates))
	return candidates[idx].sh
}

// ingestChoice is a shard a write may go to, and whether its window holds the
// clock.
type ingestChoice struct {
	sh         shard.Shard
	windowOpen bool
}

// ingestCandidate returns the live shard handle if this entry is a fresh,
// serving shard that accepts writes, and whether it is mid-upgrade. Takes
// the entry read lock. A shard with no window (dev and test stacks) is always
// open.
func (e *Shard) ingestCandidate(namespaceID uint, service string, asOf, now time.Time) (c ingestChoice, upgrading bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.writable() || e.Live == nil || e.isStaleForWrites(asOf) {
		return ingestChoice{}, false
	}
	if e.serves(namespaceID, service) {
		open := e.WindowStart.IsZero() || e.WindowEnd.IsZero() ||
			(!now.Before(e.WindowStart) && now.Before(e.WindowEnd))
		return ingestChoice{sh: e.Live, windowOpen: open}, e.State == StateUpgrading
	}
	return ingestChoice{}, false
}

// writable reports whether this shard may take writes: an INGESTING shard,
// or one UPGRADING from INGESTING. IngestingShard routes to an upgrading
// shard only when no sibling is ingesting; its write is then held across
// the volume move, since a bucket with one INGESTING shard has nowhere else
// to send it. Caller holds e.mu.
func (e *Shard) writable() bool {
	return e.State == StateIngesting ||
		(e.State == StateUpgrading && e.Upgrade.Phase != "" && e.Upgrade.Origin == StateIngesting)
}

// isStaleForWrites is isStale, except while an upgrade moves the volume:
// no instance heartbeats as the shard then, and the held write completes
// on the successor. Caller holds e.mu.
func (e *Shard) isStaleForWrites(asOf time.Time) bool {
	if e.State == StateUpgrading && e.Upgrade.Phase.VolumeMoving() {
		return false
	}
	return e.isStale(asOf)
}

// TotalIngesting returns the count of all INGESTING shards regardless of app.
func (r *ShardRegistry) TotalIngesting() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.entries {
		e.mu.RLock()
		ingesting := e.writable() && e.Live != nil
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
		ok := e.writable() && e.Live != nil && e.serves(namespaceID, service)
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

// locationEpochs snapshots every entry's locEpoch, taken before
// RefreshFromStore reads the rows.
func (r *ShardRegistry) locationEpochs() map[string]uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]uint64, len(r.entries))
	for id, e := range r.entries {
		e.mu.RLock()
		out[id] = e.locEpoch
		e.mu.RUnlock()
	}
	return out
}
