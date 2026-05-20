package logdb

import (
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// Registry-shaped methods on Provisioner. The registry is a private
// field; coordinator-side code reaches it through these delegates so
// the shard-data control plane stays behind one boundary.
//
// These are thin pass-throughs by design — every method should map 1:1
// onto a ShardRegistry method. Anything that needs more logic belongs
// on the registry itself or on a new lifecycle method.

// Add registers a ShardEntry. Used by the coordinator's cfg.Shards
// path and by tests that pre-seed fixtures.
func (p *Provisioner) Add(entry *ShardEntry) { p.registry.Add(entry) }

// Get returns the entry for id, or nil if not present.
func (p *Provisioner) Get(id string) *ShardEntry { return p.registry.Get(id) }

// AllShards returns every live Shard connection in the registry, in
// arbitrary order. Used by status RPCs.
func (p *Provisioner) AllShards() []shard.Shard { return p.registry.AllShards() }

// Entries returns a snapshot of all registry entries. Used for
// health/status reporting where the caller needs more than just the
// Shard connections.
func (p *Provisioner) Entries() []*ShardEntry { return p.registry.Entries() }

// IngestingShard returns an INGESTING shard for app via round-robin
// across matching shards, or nil if none qualify.
func (p *Provisioner) IngestingShard(app string) shard.Shard {
	return p.registry.IngestingShard(app)
}

// DefaultIngestingShard returns any non-suspended INGESTING shard.
// Used by the ingest drain's fallback path when no app-specific
// shard matches yet.
func (p *Provisioner) DefaultIngestingShard() shard.Shard {
	return p.registry.DefaultIngestingShard()
}

// TotalIngesting returns the count of INGESTING shards regardless
// of app. Used by the disk-pressure check.
func (p *Provisioner) TotalIngesting() int { return p.registry.TotalIngesting() }

// RecordIngestFailure increments the consecutive-failure counter for
// id and may flip the entry to Suspended once the threshold trips.
// Returns the new failure count.
func (p *Provisioner) RecordIngestFailure(id string) int {
	return p.registry.RecordIngestFailure(id)
}

// ResetIngestFailures clears the consecutive-failure counter for id.
// Called on a successful Ingest to keep flaky-but-recovering shards
// from drifting into Suspended.
func (p *Provisioner) ResetIngestFailures(id string) {
	p.registry.ResetIngestFailures(id)
}

// MatchingEntries returns all entries that intersect tr and serve
// app. tier clamps the time range to that subscription's retention
// window before matching.
func (p *Provisioner) MatchingEntries(tr TimeRange, app string, tier store.Tier) []*ShardEntry {
	return p.registry.MatchingEntries(tr, app, tier)
}
