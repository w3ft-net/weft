package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// stubConfigurer is a ShardConfigurer that records its Dial calls and
// hands back a fresh in-memory DuckDB shard for each id — the test seam
// standing in for a real gRPC dial. Re-homed here when christen_test.go
// was deleted in the Slice C switch.
type stubConfigurer struct {
	mu    sync.Mutex
	calls []struct {
		Addr string
		Req  *pb.ConfigureRequest
	}
	shardByID map[string]shard.Shard
}

func (s *stubConfigurer) Dial(_ context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, struct {
		Addr string
		Req  *pb.ConfigureRequest
	}{Addr: addr, Req: req})
	if s.shardByID == nil {
		s.shardByID = make(map[string]shard.Shard)
	}
	ds, err := shard.NewDuckDBShard(req.ShardId, "")
	if err != nil {
		return nil, err
	}
	s.shardByID[req.ShardId] = ds
	return ds, nil
}

// bucketStub builds a BucketManager backed by a fresh SM test store with
// the default bucket seeded to the given target/retention. Re-homed from
// window_rotation_test.go (deleted in the Slice C switch).
func bucketStub(t *testing.T, target int, retention time.Duration) *BucketManager {
	t.Helper()
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, target, retention)
	return NewBucketManager(db)
}

// seedDefaultBucket makes the "default" bucket (the Postgres migrations seed
// it; the SQLite store starts empty) with the given target, tier, and window.
func seedDefaultBucket(t *testing.T, db shardstore.Store, target int, window time.Duration) {
	t.Helper()
	def, ok, err := db.GetBucketByLabel("default")
	if err != nil {
		t.Fatalf("default bucket lookup: %v", err)
	}
	if !ok {
		def = shardstore.Bucket{Label: "default", CreatedAt: "2026-05-21T12:00:00Z", State: string(BucketStasis)}
	}
	def.TargetIngestingCount = target
	def.Tier = "small"
	def.WindowDuration = window.String()
	if err := db.SaveBucket(&def); err != nil {
		t.Fatalf("seed default bucket: %v", err)
	}
}

// Shared test helpers for logdb. Mirrors the same-named helpers in
// coordinator/liveness_test.go; both packages need them because their
// tests construct logdb-domain values (failingShard implements
// shard.Shard, recordingLauncher implements logdb.Launcher,
// mockHeartbeatStore implements logdb.HeartbeatStore). Duplicated
// rather than exported because they're pure test infrastructure with
// no runtime use.

// failingShard is a shard.Shard that fails Ingest calls on demand.
type failingShard struct {
	id       string
	mu       sync.Mutex
	failing  bool
	ingested []*pb.LogRow
}

func (s *failingShard) ID() string { return s.id }
func (s *failingShard) Query(_ context.Context, sql string, args ...any) ([]*pb.ResultRow, error) {
	return nil, nil
}
func (s *failingShard) QueryToSink(ctx context.Context, sql string, args []any, sink shard.RowSink) {}
func (s *failingShard) Close() error                                                                { return nil }
func (s *failingShard) Archive(_ context.Context, _ string) (string, []shard.PartitionStat, error) {
	return "", nil, nil
}

func (s *failingShard) Ingest(_ context.Context, rows []*pb.LogRow, rec shard.IngestRecorder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		err := fmt.Errorf("shard %s is down", s.id)
		if rec != nil {
			rec.RecordIngestFailure(s.id)
		}
		return err
	}
	s.ingested = append(s.ingested, rows...)
	if rec != nil {
		rec.ResetIngestFailures(s.id)
	}
	return nil
}

func (s *failingShard) setFailing(f bool) {
	s.mu.Lock()
	s.failing = f
	s.mu.Unlock()
}

func (s *failingShard) ingestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ingested)
}

// recordingLauncher captures TerminateInstance calls for assertions.
// Satisfies the bare ShardLauncher interface; LaunchInstance is a no-op.
type recordingLauncher struct {
	terminated []string // addr passed to TerminateInstance
}

func (r *recordingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (r *recordingLauncher) TerminateInstance(_ context.Context, _, addr string) error {
	r.terminated = append(r.terminated, addr)
	return nil
}

// mockHeartbeatStore implements the subset of store.Store used by the
// provisioner. Satisfies the provisioner's config.Store field via a
// wrapper.
type mockHeartbeatStore struct {
	heartbeats []shardstore.ShardHeartbeat
}

func (m *mockHeartbeatStore) LiveShardHeartbeats(within time.Duration) ([]shardstore.ShardHeartbeat, error) {
	return m.heartbeats, nil
}

func (m *mockHeartbeatStore) RecordHeartbeat(hb *shardstore.ShardHeartbeat) error {
	return nil
}

func (m *mockHeartbeatStore) PruneShardHeartbeats(time.Duration) error {
	return nil
}

// strPtr is a tiny helper for string-pointer literals the AWS SDK is heavy
// on. Re-homed here from launcher_asg.go when the ASG launcher was deleted in
// the EC2-direct migration; volume tests still use it for mocked EC2 output.
func strPtr(s string) *string { return &s }

func (s *failingShard) IngestBatch(ctx context.Context, b shard.Batch, rec shard.IngestRecorder) error {
	return s.Ingest(ctx, b.Rows, rec)
}

func ptr[T any](v T) *T { return &v }

// setTestStore gives p the shard-state store.
func setTestStore(p *Provisioner, db shardstore.Store) { p.SetShardStore(db) }
