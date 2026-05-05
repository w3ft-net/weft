package coordinator

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
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
	// httpfs DuckDB extension.
	LambdaFunctionURL string

	// S3Config configures parquet reads when LambdaFunctionURL is
	// empty. Nil works for local dev (filesystem parquet) but is
	// required in prod where parquet lives in S3.
	S3Config *shard.S3Config

	// AWSCredentials and AWSRegion enable SigV4 signing on Lambda
	// function URL requests. Required when LambdaFunctionURL uses
	// AWS_IAM authorization. Nil skips signing (test httptest servers).
	AWSCredentials aws.CredentialsProvider
	AWSRegion      string
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
func ReconstructShard(rec store.ShardRecord, cfg ReconstructConfig) (shard.Shard, error) {
	switch ShardState(rec.State) {
	case StateArchived:
		if rec.ParquetPath == "" {
			return nil, fmt.Errorf("ARCHIVED shard %s has no parquet_path (zombie row from rebind-timeout path)", rec.ID)
		}
		var apps []string
		_ = json.Unmarshal([]byte(rec.Apps), &apps)
		cs, err := shard.NewColdShard(rec.ID, apps, rec.ParquetPath, cfg.LambdaFunctionURL, cfg.S3Config)
		if err != nil {
			return nil, err
		}
		if cfg.AWSCredentials != nil && cfg.LambdaFunctionURL != "" {
			cs.WithAWSAuth(cfg.AWSCredentials, cfg.AWSRegion)
		}
		return cs, nil

	case StateSuspended:
		// Instance dead, awaiting replacement. No shard to dial; the
		// rebind path attaches a GRPCShard when the replacement heartbeats.
		return nil, fmt.Errorf("SUSPENDED shard %s awaiting rebind", rec.ID)

	case StateIngesting, StateReadOnly:
		if rec.Addr == "" {
			return nil, fmt.Errorf("%s shard %s has no addr (post-wipe-await-rebind)", rec.State, rec.ID)
		}
		return shard.NewGRPCShard(rec.ID, rec.Addr, cfg.ShardSecret)

	default:
		// LAUNCHING (no instance), EXPIRING (mid-rotation; volume
		// retiring), or any future state without a clear reconstruct
		// path. Caller falls through; the lifecycle path that owns
		// the state transition reattaches when ready.
		return nil, fmt.Errorf("no reconstruct path for shard %s in state %s", rec.ID, rec.State)
	}
}
