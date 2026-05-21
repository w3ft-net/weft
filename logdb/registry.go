package logdb

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// ShardState and the State* constants live in states.go now —
// see also docs/plans/asg-as-api-stateless.md for the full FSM.

// ShardEntry is the registry's per-shard metadata.
type ShardEntry struct {
	ID             string
	Apps           []string // apps this shard owns; required (an empty list means the shard is unreachable for any specific-app query)
	WindowStart    time.Time
	WindowEnd      time.Time
	State          ShardState
	Suspended      bool   // true if ingest failed repeatedly — excluded from write routing, still queryable
	IngestFailures int    // consecutive ingest failures; resets on success; triggers Suspended at SuspendFailureThreshold
	Addr           string // gRPC address for live shards
	InstanceID     string // EC2 instance ID
	ParquetPath    string // Parquet file path for archived shards
	Tier           string // instance tier (small/medium/large)
	RowCount       int64
	CreatedAt      time.Time
	EBSVolumeID    string      // EBS volume id for this shard's DuckDB; empty until volume is attached
	Shard          shard.Shard // live connection (not persisted)
	// HeartbeatToken is the per-shard bearer the Heartbeat handler
	// validates against. Minted at christen and persisted via store
	// so the coordinator can validate heartbeats after restart.
	HeartbeatToken string
	// BucketID is the surrogate FK to buckets.id (bigint since
	// migration 022). Zero means "unassigned" — christen() always
	// sets a real id before Add; tests that construct ShardEntry
	// literals without a bucket need to supply one explicitly, since
	// persist() no longer papers over the zero case.
	BucketID int64
	// LastHeartbeatAt is the wall-clock time of the most recent
	// heartbeat the provisioner observed for this shard. Updated by
	// ProcessHeartbeat (and christen on first registration). Read by
	// MatchingShards / IngestingShard to skip entries whose underlying
	// container has died and gone silent — without that filter, the
	// registry holds stale Addrs forever and Phase 2 retries against
	// them produce repeated errors. Zero (unset) is treated as fresh
	// so direct-init test fixtures don't get filtered out.
	LastHeartbeatAt time.Time
}

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
// fresh — see the LastHeartbeatAt field comment.
func (e *ShardEntry) isStale() bool {
	if e.LastHeartbeatAt.IsZero() {
		return false
	}
	return time.Since(e.LastHeartbeatAt) > heartbeatStaleThreshold
}

// hasApp returns true if this entry owns the given app. Both exact
// match and namespace-prefix match (e.g. e.Apps={"weft/coordinator"}
// matches app="weft" — same namespace) count.
func (e *ShardEntry) hasApp(app string) bool {
	// Extract namespace from query: weft/coordinator → weft, weft → weft
	queryNS := app
	if idx := strings.Index(queryNS, "/"); idx >= 0 {
		queryNS = queryNS[:idx]
	}

	for _, a := range e.Apps {
		if a == app {
			return true
		}
		entryNS := a
		if idx := strings.Index(entryNS, "/"); idx >= 0 {
			entryNS = entryNS[:idx]
		}
		if entryNS == queryNS {
			return true
		}
	}
	return false
}

// RegistryStore is the subset of *store.Store that ShardRegistry needs.
// Extracted so tests can swap in an in-memory fake instead of postgres.
type RegistryStore interface {
	SaveShard(rec *store.ShardRecord) error
	DeleteShard(id string) error
	ListShards() ([]store.ShardRecord, error)
	GetShard(id string) (store.ShardRecord, bool, error)
	TouchHeartbeat(id string, rowCount int64) error
}

// ShardRegistry tracks all shard entries. Thread-safe.
// If a Store is set, mutations are persisted.
type ShardRegistry struct {
	mu      sync.RWMutex
	entries map[string]*ShardEntry
	robin   atomic.Uint64 // round-robin counter for write distribution
	store   RegistryStore // optional persistence (nil in tests)
}

// NewShardRegistry creates an empty registry.
func NewShardRegistry() *ShardRegistry {
	return &ShardRegistry{
		entries: make(map[string]*ShardEntry),
	}
}

// SetStore enables persistence. Accepts *store.Store in production and
// an in-memory RegistryStore in tests.
func (r *ShardRegistry) SetStore(s RegistryStore) {
	r.store = s
}

// Add registers a shard entry and persists it.
func (r *ShardRegistry) Add(entry *ShardEntry) {
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
func (r *ShardRegistry) persist(entry *ShardEntry) {
	if r.store == nil {
		return
	}
	appsJSON, _ := json.Marshal(entry.Apps)
	if err := r.store.SaveShard(&store.ShardRecord{
		ID:             entry.ID,
		Apps:           string(appsJSON),
		WindowStart:    entry.WindowStart.Format(time.RFC3339),
		WindowEnd:      entry.WindowEnd.Format(time.RFC3339),
		State:          string(entry.State),
		Addr:           entry.Addr,
		InstanceID:     entry.InstanceID,
		ParquetPath:    entry.ParquetPath,
		Tier:           entry.Tier,
		RowCount:       entry.RowCount,
		CreatedAt:      entry.CreatedAt.Format(time.RFC3339),
		EBSVolumeID:    entry.EBSVolumeID,
		HeartbeatToken: entry.HeartbeatToken,
		BucketID:       entry.BucketID,
	}); err != nil {
		log.Printf("registry: persist shard %s: %v", entry.ID, err)
	}
}

// PersistState saves the current state of an entry to the store.
// Call after modifying State, ParquetPath, RowCount, etc.
func (r *ShardRegistry) PersistState(entry *ShardEntry) {
	r.persist(entry)
}

// TouchHeartbeat persists only heartbeat-owned fields (row_count)
// without overwriting lifecycle-owned fields like window bounds,
// state, or apps. Prevents the heartbeat path from clobbering
// manual DB edits or reconciler-driven state changes.
func (r *ShardRegistry) TouchHeartbeat(entry *ShardEntry) {
	if r.store == nil {
		return
	}
	if err := r.store.TouchHeartbeat(entry.ID, entry.RowCount); err != nil {
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
	records, err := r.store.ListShards()
	if err != nil {
		log.Printf("registry: RefreshFromStore: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range records {
		entry, ok := r.entries[rec.ID]
		if !ok {
			continue
		}
		entry.State = ShardState(rec.State)
		if ws, err := time.Parse(time.RFC3339, rec.WindowStart); err == nil {
			entry.WindowStart = ws
		}
		if we, err := time.Parse(time.RFC3339, rec.WindowEnd); err == nil {
			entry.WindowEnd = we
		}
		if ca, err := time.Parse(time.RFC3339, rec.CreatedAt); err == nil {
			entry.CreatedAt = ca
		}
		entry.Addr = rec.Addr
		entry.InstanceID = rec.InstanceID
		entry.ParquetPath = rec.ParquetPath
		entry.Tier = rec.Tier
		entry.RowCount = rec.RowCount
		entry.EBSVolumeID = rec.EBSVolumeID
		entry.HeartbeatToken = rec.HeartbeatToken
		entry.BucketID = rec.BucketID
		if rec.Apps != "" && rec.Apps != "null" {
			var apps []string
			if err := json.Unmarshal([]byte(rec.Apps), &apps); err == nil {
				entry.Apps = apps
			}
		}
	}
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
func (r *ShardRegistry) LoadFromStore(reconstruct func(rec store.ShardRecord) (shard.Shard, error)) error {
	if r.store == nil {
		return nil
	}
	records, err := r.store.ListShards()
	if err != nil {
		return err
	}
	for _, rec := range records {
		// reconstruct's error is informational — a non-ARCHIVED row
		// (or one we can't open data for yet) still belongs in the
		// registry. Drop the Shard pointer; everything else loads.
		sh, _ := reconstruct(rec)
		var apps []string
		json.Unmarshal([]byte(rec.Apps), &apps)
		windowStart, _ := time.Parse(time.RFC3339, rec.WindowStart)
		windowEnd, _ := time.Parse(time.RFC3339, rec.WindowEnd)
		createdAt, _ := time.Parse(time.RFC3339, rec.CreatedAt)

		r.mu.Lock()
		r.entries[rec.ID] = &ShardEntry{
			ID:             rec.ID,
			Apps:           apps,
			WindowStart:    windowStart,
			WindowEnd:      windowEnd,
			State:          ShardState(rec.State),
			Addr:           rec.Addr,
			InstanceID:     rec.InstanceID,
			ParquetPath:    rec.ParquetPath,
			Tier:           rec.Tier,
			RowCount:       rec.RowCount,
			CreatedAt:      createdAt,
			EBSVolumeID:    rec.EBSVolumeID,
			Shard:          sh,
			HeartbeatToken: rec.HeartbeatToken,
			BucketID:       rec.BucketID,
		}
		r.mu.Unlock()
	}
	return nil
}

// Remove deletes a shard entry and closes its connection.
func (r *ShardRegistry) Remove(id string) {
	r.mu.Lock()
	if e, ok := r.entries[id]; ok {
		if e.Shard != nil {
			e.Shard.Close()
		}
		delete(r.entries, id)
	}
	r.mu.Unlock()

	if r.store != nil {
		r.store.DeleteShard(id)
	}
}

// AddAppToShard appends app to the given shard's Apps list under
// the registry's write lock. Idempotent (no-op if the app is
// already present) and state-gated (READ_ONLY/ARCHIVED shards'
// apps stay frozen — they never accepted writes for the app, so
// including them in query routing would widen fan-out).
//
// Returns (found, added):
//   - found=false means the shard isn't in the registry
//   - found=true, added=false means the op was a no-op (already
//     present or state != INGESTING)
//   - found=true, added=true means the shard's Apps list grew
//
// Callers that mutate entry.Apps directly bypass the lock and
// race with IngestingShard/MatchingShards readers — don't.
func (r *ShardRegistry) AddAppToShard(id, app string) (found bool, added bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[id]
	if !ok {
		return false, false
	}
	if entry.State != StateIngesting {
		return true, false
	}
	for _, a := range entry.Apps {
		if a == app {
			return true, false
		}
	}
	entry.Apps = append(entry.Apps, app)
	r.persist(entry)
	return true, true
}

// Suspend marks a shard as suspended (ingest failed). Suspended shards
// are excluded from write routing but still participate in queries.
func (r *ShardRegistry) Suspend(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.Suspended = true
	}
}

// RecordIngestFailure increments the consecutive-failure counter for the
// given shard and marks it Suspended once it hits SuspendFailureThreshold.
// Returns (count, threshold) so callers (Shard.Ingest impls) can format
// an error message that names the resulting state without re-importing
// the threshold constant. Pair with ResetIngestFailures on successful
// ingest so transient failures don't accumulate forever.
func (r *ShardRegistry) RecordIngestFailure(id string) (count, threshold int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return 0, SuspendFailureThreshold
	}
	e.IngestFailures++
	if e.IngestFailures >= SuspendFailureThreshold {
		e.Suspended = true
	}
	return e.IngestFailures, SuspendFailureThreshold
}

// ResetIngestFailures clears the consecutive-failure counter for a shard
// after a successful ingest. Does not alter Suspended — an explicit
// heartbeat-based unsuspend handles that side.
func (r *ShardRegistry) ResetIngestFailures(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.IngestFailures = 0
	}
}

// Get returns a shard entry by ID, or nil.
func (r *ShardRegistry) Get(id string) *ShardEntry {
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
		if e.Addr == addr {
			return true
		}
	}
	return false
}

// MatchingShards returns shards whose time windows overlap the given range
// AND whose app set includes the given app. If app is "", all shards with
// overlapping time windows are returned.
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
func (r *ShardRegistry) MatchingEntries(tr TimeRange, app string, tier store.Tier) []*ShardEntry {
	if cap := time.Now().Add(-store.LogRetention(tier)); tr.Start.Before(cap) {
		tr.Start = cap
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*ShardEntry
	for _, e := range r.entries {
		if e.Addr == "" && e.Shard == nil {
			continue
		}
		// SUSPENDED (instance dead, Addr/InstanceID cleared, awaiting
		// rebind — see provisioner's stale-heartbeat path and
		// ReconstructShard) is unqueryable: a SUSPENDED entry keeps a
		// stale non-nil Shard pointer so the Addr==""/Shard==nil guard
		// below doesn't catch it, and routing it only yields "no
		// grpc_addr or lambda_url".
		if e.State == StateSuspended {
			continue
		}
		if e.isStale() {
			continue
		}
		// An ARCHIVED (cold-Lambda) or READ_ONLY shard with zero rows
		// has nothing to return; dialing it is pure waste — a cold dial
		// is a Lambda round-trip. Live INGESTING shards are exempt: a
		// transient 0 count there just means it hasn't filled yet.
		if e.RowCount == 0 && (e.State == StateArchived || e.State == StateReadOnly) {
			continue
		}
		hasWindow := !e.WindowStart.IsZero() && !e.WindowEnd.IsZero()
		if hasWindow && (!e.WindowStart.Before(tr.End) || !e.WindowEnd.After(tr.Start)) {
			continue
		}
		if app != "" && !e.hasApp(app) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// IngestingShard returns an INGESTING shard for the given app, selected
// via round-robin across all matching INGESTING shards. This distributes
// writes across N concurrent writable shards for the same app.
func (r *ShardRegistry) IngestingShard(app string) shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var candidates []shard.Shard
	for _, e := range r.entries {
		if e.State != StateIngesting || e.Shard == nil || e.Suspended || e.isStale() {
			continue
		}
		if e.hasApp(app) {
			candidates = append(candidates, e.Shard)
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	idx := r.robin.Add(1) % uint64(len(candidates))
	return candidates[idx]
}

// TotalIngesting returns the count of all INGESTING shards regardless of app.
func (r *ShardRegistry) TotalIngesting() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.entries {
		if e.State == StateIngesting && e.Shard != nil {
			n++
		}
	}
	return n
}

// IngestingCount returns the number of INGESTING shards for the given app.
func (r *ShardRegistry) IngestingCount(app string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.entries {
		if e.State == StateIngesting && e.Shard != nil && e.hasApp(app) {
			n++
		}
	}
	return n
}

// Entries returns a snapshot of all entries (for health/status reporting).
func (r *ShardRegistry) Entries() []*ShardEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*ShardEntry, 0, len(r.entries))
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
