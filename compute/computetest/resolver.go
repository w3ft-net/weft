// Package computetest is the test-only complement to compute. It
// exports an InProcessResolver that hands compute a direct
// shard.Shard pointer instead of dialing one over the network — the
// missing case that registryPool used to cover by reaching into the
// coordinator's registry.
//
// Production code must never import this package. The split is by
// import graph rather than build tags so the boundary is enforced by
// `go vet ./...` (an unused import) and by code review (any
// computetest import in cmd/ or non-test .go files is a bug).
package computetest

import (
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/shard"
)

// InProcessResolver wraps a shard.Shard handle that already exists
// in this process — typically a *shard.DuckDBShard built directly by
// a test. Dial just returns the wrapped handle; ShardID returns the
// id the test minted. Compute's LocalComputePool, once a test calls
// pool.RegisterResolver(NewInProcessResolver(id, sh)), will route
// every descriptor with that ShardID through this resolver instead
// of trying to dial GrpcAddr/LambdaUrl.
type InProcessResolver struct {
	id string
	sh shard.Shard
}

// NewInProcessResolver returns a Resolver that, when Dialed, hands
// compute the given shard.Shard handle.
func NewInProcessResolver(id string, sh shard.Shard) *InProcessResolver {
	return &InProcessResolver{id: id, sh: sh}
}

// ShardID returns the id this resolver claims. Used as the override
// key in LocalComputePool.overrides.
func (r *InProcessResolver) ShardID() string { return r.id }

// Dial returns the wrapped shard.Shard. Never errors; the handle is
// already constructed.
func (r *InProcessResolver) Dial() (shard.Shard, error) { return r.sh, nil }

// Compile-time check that InProcessResolver satisfies compute.Resolver.
var _ compute.Resolver = (*InProcessResolver)(nil)
