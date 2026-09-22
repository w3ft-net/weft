package service

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	weftsdk "github.com/w3ft-net/weft-go"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// captureShardLog mirrors shard/cold_shard_observability_test.go's
// captureLog helper — Query uses stdlib log.Printf.
func captureShardLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// queryCaptureStream is a minimal ShardQuery_QueryServer, modeled on
// coordinator/service's traceCaptureStream.
type queryCaptureStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *queryCaptureStream) Context() context.Context { return s.ctx }
func (s *queryCaptureStream) Send(*pb.ResultRow) error { return nil }

func newQueryTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shard.duckdb")
	srv := NewBlankServer(dir, dbPath)
	if _, err := srv.Configure(context.Background(), &pb.ConfigureRequest{
		ShardId:     "trace-log-test",
		Services:    []string{"weft"},
		WindowStart: time.Now().Add(-time.Hour).Format(time.RFC3339),
		WindowEnd:   time.Now().Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	ds, err := srv.getShard()
	if err != nil {
		t.Fatalf("getShard: %v", err)
	}
	if err := ds.Ingest([]*pb.LogRow{
		{Raw: "hello", Namespace: "acme", Service: "app", Kind: "main", Customer: "acme", TsUnixNs: time.Now().UnixNano()},
	}, nil); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return srv
}

// TestQuery_SilentWhenUntraced: unlike a cold-shard query (always
// logged — see shard/cold_shard_observability_test.go), a live shard's
// query line stays fully silent unless a trace ID is ambient — this is
// the hot, frequent path, so unconditional per-query logging here
// would be real log spam in a way cold-shard logging isn't.
func TestQuery_SilentWhenUntraced(t *testing.T) {
	srv := newQueryTestServer(t)

	req := &pb.QueryRequest{Sql: "SELECT raw, ts FROM logs WHERE customer = 'acme'"}
	stream := &queryCaptureStream{ctx: context.Background()}

	out := captureShardLog(t, func() {
		if err := srv.Query(req, stream); err != nil {
			t.Fatalf("Query: %v", err)
		}
	})

	if strings.TrimSpace(out) != "" {
		t.Fatalf("untraced query should not log; got %q", out)
	}
}

// TestQuery_LogsTraceIDWhenPresent: the mirror-image positive case —
// an ambient trace ID (ferried as weft-trace-id gRPC metadata by
// compute, mirroring the api→coordinator boundary) produces exactly
// one log line naming it.
func TestQuery_LogsTraceIDWhenPresent(t *testing.T) {
	srv := newQueryTestServer(t)

	req := &pb.QueryRequest{Sql: "SELECT raw, ts FROM logs WHERE customer = 'acme'"}
	ctx := weftsdk.ContextWithTraceID(context.Background(), "shard-trace-abc")
	stream := &queryCaptureStream{ctx: ctx}

	out := captureShardLog(t, func() {
		if err := srv.Query(req, stream); err != nil {
			t.Fatalf("Query: %v", err)
		}
	})

	if !strings.Contains(out, "trace_id=shard-trace-abc") {
		t.Fatalf("traced query should log the trace id; got %q", out)
	}
}
