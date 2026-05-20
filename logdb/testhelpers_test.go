package logdb

import (
	"context"
	"fmt"
	"sync"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

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
func (s *failingShard) Query(sql string, args ...any) ([]*pb.ResultRow, error) {
	return nil, nil
}
func (s *failingShard) QueryToSink(sql string, args []any, sink shard.RowSink) {}
func (s *failingShard) Close() error                                           { return nil }
func (s *failingShard) Archive(_ context.Context, _ string) (string, error)    { return "", nil }

func (s *failingShard) Ingest(rows []*pb.LogRow, rec shard.IngestRecorder) error {
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

// recordingLauncher captures TerminateInstance / TerminateForReplacement
// calls for assertions. Satisfies ShardLauncher and RecoveryTerminator;
// LaunchInstance is a no-op.
type recordingLauncher struct {
	terminated           []string // addr passed to TerminateInstance (ASG decrement semantics)
	terminatedForReplace []string // instance id passed to TerminateForReplacement (ASG replace semantics)
	replacementErr       error
}

func (r *recordingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (r *recordingLauncher) TerminateInstance(_ context.Context, _, addr string) error {
	r.terminated = append(r.terminated, addr)
	return nil
}

func (r *recordingLauncher) TerminateForReplacement(_ context.Context, instanceID string) error {
	r.terminatedForReplace = append(r.terminatedForReplace, instanceID)
	return r.replacementErr
}

// mockHeartbeatStore implements the subset of store.Store used by the
// provisioner. Satisfies the provisioner's config.Store field via a
// wrapper.
type mockHeartbeatStore struct {
	heartbeats []store.ShardHeartbeat
}

func (m *mockHeartbeatStore) LiveShardHeartbeats(since string) ([]store.ShardHeartbeat, error) {
	return m.heartbeats, nil
}

func (m *mockHeartbeatStore) InsertHeartbeat(hb *store.ShardHeartbeat) error {
	return nil
}
