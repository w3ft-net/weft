package coordinator

import (
	"sync"
	"time"

	"github.com/kevlarcon/weft/shard"
)

// ShardState represents the lifecycle state of a shard.
type ShardState string

const (
	StateLaunching ShardState = "LAUNCHING"
	StateIngesting ShardState = "INGESTING"
	StateReadOnly  ShardState = "READ_ONLY"
	StateExpiring  ShardState = "EXPIRING"
)

// ShardEntry is the registry's per-shard metadata.
type ShardEntry struct {
	ID          string
	Apps        []string   // apps this shard owns; empty = all apps (legacy/shared)
	WindowStart time.Time
	WindowEnd   time.Time
	State       ShardState
	Addr        string
	InstanceID  string
	RowCount    int64
	CreatedAt   time.Time
	Shard       shard.Shard // live connection (not persisted)
}

// hasApp returns true if this entry owns the given app, or if the entry
// is a legacy/shared shard with an empty Apps list (matches all apps).
func (e *ShardEntry) hasApp(app string) bool {
	if len(e.Apps) == 0 {
		return true // legacy: empty = all apps
	}
	for _, a := range e.Apps {
		if a == app {
			return true
		}
	}
	return false
}

// ShardRegistry tracks all shard entries. Thread-safe.
type ShardRegistry struct {
	mu      sync.RWMutex
	entries map[string]*ShardEntry
}

// NewShardRegistry creates an empty registry.
func NewShardRegistry() *ShardRegistry {
	return &ShardRegistry{
		entries: make(map[string]*ShardEntry),
	}
}

// Add registers a shard entry.
func (r *ShardRegistry) Add(entry *ShardEntry) {
	r.mu.Lock()
	r.entries[entry.ID] = entry
	r.mu.Unlock()
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
}

// Get returns a shard entry by ID, or nil.
func (r *ShardRegistry) Get(id string) *ShardEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[id]
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
		if !e.WindowStart.Before(tr.End) || !e.WindowEnd.After(tr.Start) {
			continue
		}
		if app != "" && !e.hasApp(app) {
			continue
		}
		out = append(out, e.Shard)
	}
	return out
}

// IngestingShard returns the live Shard connection for the current INGESTING
// shard that owns the given app. Returns nil if none found.
func (r *ShardRegistry) IngestingShard(app string) shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, e := range r.entries {
		if e.State != StateIngesting || e.Shard == nil {
			continue
		}
		if e.hasApp(app) {
			return e.Shard
		}
	}
	return nil
}

// DefaultIngestingShard returns any INGESTING shard, preferring one with
// an empty Apps list (shared/catch-all). Fallback for unknown apps.
func (r *ShardRegistry) DefaultIngestingShard() shard.Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Prefer shared (empty Apps) shard.
	for _, e := range r.entries {
		if e.State == StateIngesting && e.Shard != nil && len(e.Apps) == 0 {
			return e.Shard
		}
	}
	// Fallback: any ingesting shard.
	for _, e := range r.entries {
		if e.State == StateIngesting && e.Shard != nil {
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
