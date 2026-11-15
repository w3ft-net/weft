//go:build test

package logdb

// Test fixture helpers. Lives in a normal .go file (not _test.go) so
// other packages' tests can import these — _test.go files don't
// export to outside packages.
//
// Guarded by `//go:build test`: production `go build` excludes this
// file entirely, so SeedShard cannot be linked into a production
// binary even if someone accidentally references it. Tests must run
// with `-tags test` (see the Makefile `test` target).
//
// Production code paths use the launcher → christen → registry.Add
// chain; that's the only legitimate way to add shards in a running
// coordinator.

// SeedShard installs entry directly into the provisioner's registry,
// bypassing the launcher and christen handshake. Strictly for test
// fixture setup — production shards arrive through ProcessHeartbeat.
//
// The registry field is unexported, so callers outside logdb can't
// reach it directly; this function is the seam that lets coordinator-
// side tests pre-populate the registry with synthetic entries.
func SeedShard(p *Provisioner, entry *Shard) {
	p.registry.Add(entry)
}

// GetEntry reads back a seeded entry by id. The companion to
// SeedShard for tests that need to inspect or mutate the synthetic
// entry after planting it (e.g. flipping LastHeartbeatAt to drive
// stale-handling branches). Returns nil if no entry with that id
// is registered.
func GetEntry(p *Provisioner, id string) *Shard {
	return p.registry.Get(id)
}
