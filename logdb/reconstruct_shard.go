package logdb

import (
	"fmt"

	"github.com/kevlarcon/weft/shard"
)

// ReconstructConfig wires up the dependencies ReconstructShard needs
// to dial live shards or open archived ones. Populated once in
// cmd/coordinator/main.go from the same env vars / SSM blob keys
// the rest of the coordinator uses; passed by value into each
// LoadFromStore reconstruct call.
type ReconstructConfig struct {
	// ShardSecret is the SHARD_SECRET bearer token attached to every
	// outbound coord→shard gRPC call. Empty in test fixtures where
	// the shard server runs without an interceptor.
	ShardSecret string

	// LambdaFunctionURL, if set, routes ARCHIVED-shard reads through
	// a Lambda that opens the parquet snapshot. Empty falls back to
	// the in-process ColdShard which opens parquet directly via the
	// httpfs DuckDB extension. The ColdShard built here is dead
	// inventory — the actual archived-shard query goes through
	// compute, which dials its own ColdShard from the descriptor.
	// Carried only because LoadFromStore expects a non-nil Shard
	// pointer for state-based cleanup.
	LambdaFunctionURL string
}

// ReconstructShard turns a persisted shard_records row into a live
// shard.Shard pointer for LoadFromStore. The previous reconstruct
// (inline in cmd/coordinator/main.go) only handled ARCHIVED rows,
// leaving every live row in the registry with Shard=nil and only
// fillable lazily via the heartbeat-adopt path. That left a window
// after coordinator restart where MatchingShards (which skips
// nil-Shard entries) returned empty and queries silently dropped to
// zero rows for shards that were running just fine.
//
// Now: every state with a known address dials a GRPCShard
// immediately. grpc.NewClient is lazy — no actual TCP at boot —
// so a hundred shards in the DB don't block the coordinator's
// readiness; queries just work the moment the registry finishes
// loading.
//
// States that legitimately can't reconstruct (LAUNCHING with no
// instance, post-wipe-rebind with empty addr, zombie ARCHIVED with
// no parquet) return an error. LoadFromStore ignores it and leaves
// Shard=nil; the heartbeat path's christen / rebind branches fill
// those entries when the live shard reappears.
func ReconstructShard(sh *Shard, cfg ReconstructConfig) (shard.Shard, error) {
	switch sh.State {
	case StateArchived:
		if sh.ParquetPath == "" {
			return nil, fmt.Errorf("ARCHIVED shard %s has no parquet_path (zombie row from rebind-timeout path)", sh.ID)
		}
		// ColdShard's apps arg is dead metadata (the cold-query path
		// ignores it); the registry sets the entry's real service
		// membership from shard_services at LoadFromStore.
		return shard.NewColdShard(sh.ID, nil, sh.ParquetPath, cfg.LambdaFunctionURL, nil)

	case StateSuspended, StateUpgrading:
		// Instance dead (SUSPENDED) or being intentionally rolled
		// (UPGRADING); either way no shard to dial. The rebind path
		// attaches a GRPCShard when the replacement heartbeats.
		return nil, fmt.Errorf("%s shard %s awaiting rebind", sh.State, sh.ID)

	case StateIngesting, StateReadOnly:
		if sh.Addr == "" {
			return nil, fmt.Errorf("%s shard %s has no addr (post-wipe-await-rebind)", sh.State, sh.ID)
		}
		return shard.NewGRPCShard(sh.ID, sh.Addr, cfg.ShardSecret)

	default:
		// LAUNCHING (no instance), EXPIRING (mid-rotation; volume
		// retiring), or any future state without a clear reconstruct
		// path. Caller falls through; the lifecycle path that owns
		// the state transition reattaches when ready.
		return nil, fmt.Errorf("no reconstruct path for shard %s in state %s", sh.ID, sh.State)
	}
}
