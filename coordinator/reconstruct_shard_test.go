package coordinator

import (
	"strings"
	"testing"

	"github.com/kevlarcon/weft/store"
)

// TestReconstructShard_IngestingDials covers the post-restart bootstrap
// path: a persisted INGESTING row with a known address must come back
// from LoadFromStore with a live shard.Shard pointer attached. Without
// it, MatchingEntries (registry.go's nil-Shard guard) silently skips
// the entry and queries return zero rows until a heartbeat arrives —
// the symptom that motivated this work.
func TestReconstructShard_IngestingDials(t *testing.T) {
	rec := store.ShardRecord{
		ID:    "shard-live-ingest",
		State: string(StateIngesting),
		Addr:  "10.0.0.1:9000",
		Apps:  `["weft"]`,
	}
	sh, err := ReconstructShard(rec, ReconstructConfig{ShardSecret: "test-secret"})
	if err != nil {
		t.Fatalf("ReconstructShard: %v", err)
	}
	if sh == nil {
		t.Fatal("Shard pointer nil after reconstruct of an INGESTING row with a known addr")
	}
	if sh.ID() != rec.ID {
		t.Errorf("Shard.ID() = %q, want %q", sh.ID(), rec.ID)
	}
}

// TestReconstructShard_ReadOnlyDials: READ_ONLY shards (current
// window's predecessor, still queryable but no new writes) carry an
// addr and must also dial.
func TestReconstructShard_ReadOnlyDials(t *testing.T) {
	rec := store.ShardRecord{
		ID:    "shard-readonly",
		State: string(StateReadOnly),
		Addr:  "10.0.0.2:9000",
		Apps:  `["weft"]`,
	}
	sh, err := ReconstructShard(rec, ReconstructConfig{ShardSecret: "test-secret"})
	if err != nil {
		t.Fatalf("ReconstructShard: %v", err)
	}
	if sh == nil {
		t.Fatal("READ_ONLY shard came back with nil Shard pointer; queries against archived-but-not-yet-cold data won't reach it")
	}
}

// TestReconstructShard_IngestingNoAddr: a row in the wipe-and-await-
// rebind state has its Addr blanked. There's no live instance to dial,
// so the function returns (nil, error) and LoadFromStore leaves the
// registry entry's Shard pointer nil. The heartbeat-adopt path's
// rebind branch fills it in when the replacement instance heartbeats.
func TestReconstructShard_IngestingNoAddr(t *testing.T) {
	rec := store.ShardRecord{
		ID:    "shard-mid-rebind",
		State: string(StateIngesting),
		Addr:  "", // wiped, awaiting rebind
		Apps:  `["weft"]`,
	}
	sh, err := ReconstructShard(rec, ReconstructConfig{ShardSecret: "test-secret"})
	if err == nil {
		t.Fatal("expected error for ingesting row with empty addr; got nil")
	}
	if sh != nil {
		t.Errorf("Shard pointer non-nil for empty-addr row: %v", sh)
	}
}

// TestReconstructShard_LaunchingNoDial: LAUNCHING rows have no
// instance yet (the ASG hasn't claimed the lifecycle hook). Dialling
// would be premature; the christen path attaches the Shard once the
// instance heartbeats with the minted id.
func TestReconstructShard_LaunchingNoDial(t *testing.T) {
	rec := store.ShardRecord{
		ID:    "shard-launching",
		State: string(StateLaunching),
		Addr:  "", // no instance yet
		Apps:  `["weft"]`,
	}
	sh, err := ReconstructShard(rec, ReconstructConfig{ShardSecret: "test-secret"})
	if err == nil {
		t.Fatal("expected error for LAUNCHING row; got nil")
	}
	if sh != nil {
		t.Errorf("Shard pointer non-nil for LAUNCHING row")
	}
}

// TestReconstructShard_ArchivedNoParquet: a "zombie archive" — entry
// transitioned to ARCHIVED via reapDeadShards' rebind-timeout branch
// without parquet export. Nothing to dial, nothing to read; surfaces
// as an error so LoadFromStore leaves Shard=nil. MatchingEntries
// already skips ARCHIVED entries in MatchingEntries' state check
// regardless, so the nil pointer is harmless in the query path.
func TestReconstructShard_ArchivedNoParquet(t *testing.T) {
	rec := store.ShardRecord{
		ID:          "shard-zombie",
		State:       string(StateArchived),
		ParquetPath: "", // zombie path never exported
		Apps:        `["weft"]`,
	}
	sh, err := ReconstructShard(rec, ReconstructConfig{ShardSecret: "test-secret"})
	if err == nil {
		t.Fatal("expected error for ARCHIVED row with no parquet path; got nil")
	}
	if sh != nil {
		t.Errorf("Shard pointer non-nil for zombie ARCHIVED row")
	}
	if !strings.Contains(err.Error(), "parquet") {
		t.Errorf("error should mention missing parquet path; got: %v", err)
	}
}

// TestReconstructShard_ArchivedWithParquet: the canonical cold-shard
// reconstruct, the only path that worked before this change.
func TestReconstructShard_ArchivedWithParquet(t *testing.T) {
	rec := store.ShardRecord{
		ID:          "shard-cold",
		State:       string(StateArchived),
		ParquetPath: "s3://weft-cold/weft/shard-cold.parquet",
		Apps:        `["weft"]`,
	}
	sh, err := ReconstructShard(rec, ReconstructConfig{
		ShardSecret:       "test-secret",
		LambdaFunctionURL: "https://lambda.example/invoke",
	})
	if err != nil {
		t.Fatalf("ReconstructShard: %v", err)
	}
	if sh == nil {
		t.Fatal("ARCHIVED row with parquet path came back nil; cold queries would skip the entry")
	}
}
