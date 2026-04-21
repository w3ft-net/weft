package coordinator

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kevlarcon/weft/store"
	"github.com/kevlarcon/weft/shard"
)

// ShardState represents the lifecycle state of a shard.
type ShardState string

const (
	StateLaunching ShardState = "LAUNCHING"
	StateIngesting ShardState = "INGESTING"
	StateReadOnly  ShardState = "READ_ONLY"
	StateExpiring  ShardState = "EXPIRING"
	StateArchived  ShardState = "ARCHIVED"
)

// ShardEntry is the registry's per-shard metadata.
type ShardEntry struct {
	ID             string
	Apps           []string // apps this shard owns; empty = all apps (legacy/shared)
	WindowStart    time.Time
	WindowEnd      time.Time
	State          ShardState
	Suspended      bool        // true if ingest failed repeatedly — excluded from write routing, still queryable
	IngestFailures int         // consecutive ingest failures; resets on success; triggers Suspended at suspendFailureThreshold
	Addr           string      // gRPC address for live shards
	InstanceID     string      // EC2 instance ID
	ParquetPath    string      // Parquet file path for archived shards
	Tier           string      // instance tier (small/medium/large)
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
}

// suspendFailureThreshold is the number of consecutive ingest failures
// that marks a shard as suspended. One flaky RPC shouldn't take a shard
// out of rotation; a sustained run of failures should.
const suspendFailureThreshold = 3

// hasApp returns true if this entry owns the given app. Empty Apps
// means the shard is a catch-all (heartbeat-adopted shards start with
// no app assignments and accept everything).
func (e *ShardEntry) hasApp(app string) bool {
	if len(e.Apps) == 0 {
		return true // catch-all shard
	}

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
	// GetShard returns the record for id, or (zero, false) if absent.
	// Used by the adopt path so it can preserve WindowStart/End/
	// HeartbeatToken/Apps when creating a ShardEntry for a shard
	// the registry has no in-memory record of but the DB does.
	GetShard(id string) (store.ShardRecord, bool, error)
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
func (r *ShardRegistry) persist(entry *ShardEntry) {
	if r.store == nil {
		return
	}
	appsJSON, _ := json.Marshal(entry.Apps)
	// BucketID is required (NOT NULL FK). No silent fallback —
	// callers must set it explicitly before Add/PersistState. A zero
	// value here means a bug upstream; surface it via the DB's
	// constraint failure rather than paper over it with a lookup.
	r.store.SaveShard(&store.ShardRecord{
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
	})
}

// PersistState saves the current state of an entry to the store.
// Call after modifying State, ParquetPath, RowCount, etc.
func (r *ShardRegistry) PersistState(entry *ShardEntry) {
	r.persist(entry)
}

// LoadFromStore loads archived (and optionally other) shard records from
// the store and calls the provided function to reconstruct each shard.
// The reconstruct function should create the appropriate Shard implementation
// (ColdShard for ARCHIVED, GRPCShard for live) and return it.
func (r *ShardRegistry) LoadFromStore(reconstruct func(rec store.ShardRecord) (shard.Shard, error)) error {
	if r.store == nil {
		return nil
	}
	records, err := r.store.ListShards()
	if err != nil {
		return err
	}
	for _, rec := range records {
		sh, err := reconstruct(rec)
		if err != nil {
			continue // skip unrecoverable entries
		}
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
// given shard and marks it Suspended once it hits suspendFailureThreshold.
// Returns the new count. Callers should pair this with ResetIngestFailures
// on successful ingest so transient failures don't accumulate forever.
func (r *ShardRegistry) RecordIngestFailure(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return 0
	}
	e.IngestFailures++
	if e.IngestFailures >= suspendFailureThreshold {
		e.Suspended = true
	}
	return e.IngestFailures
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
func (r *ShardRegistry) MatchingShards(tr TimeRange, app string) []shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []shard.Shard
	for _, e := range r.entries {
		if e.Shard == nil {
			continue
		}
		if e.State == StateExpiring {
			continue
		}
		// Zero-value windows = unbounded (heartbeat-adopted shards).
		hasWindow := !e.WindowStart.IsZero() && !e.WindowEnd.IsZero()
		if hasWindow && (!e.WindowStart.Before(tr.End) || !e.WindowEnd.After(tr.Start)) {
			continue
		}
		if app != "" && !e.hasApp(app) {
			continue
		}
		out = append(out, e.Shard)
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
		if e.State != StateIngesting || e.Shard == nil || e.Suspended {
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

// DefaultIngestingShard returns any INGESTING shard, preferring one with
// an empty Apps list (shared/catch-all). Fallback for unknown apps.
func (r *ShardRegistry) DefaultIngestingShard() shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Prefer shared (empty Apps) shard.
	for _, e := range r.entries {
		if e.State == StateIngesting && e.Shard != nil && !e.Suspended && len(e.Apps) == 0 {
			return e.Shard
		}
	}
	// Fallback: any non-suspended ingesting shard.
	for _, e := range r.entries {
		if e.State == StateIngesting && e.Shard != nil && !e.Suspended {
			return e.Shard
		}
	}
	return nil
}

// AllShards returns all live Shard connections.
func (r *ShardRegistry) AllShards() []shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []shard.Shard
	for _, e := range r.entries {
		if e.Shard != nil {
			out = append(out, e.Shard)
		}
	}
	return out
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
