package shard

import (
	"context"
	"net"
	"sync"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// mdCaptureServer records the incoming metadata of every Query/Ingest.
type mdCaptureServer struct {
	pb.UnimplementedShardQueryServer
	mu    sync.Mutex
	query []metadata.MD
	ingst []metadata.MD
}

func (s *mdCaptureServer) Query(_ *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.ResultRow]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	s.mu.Lock()
	s.query = append(s.query, md)
	s.mu.Unlock()
	return stream.Send(&pb.ResultRow{Raw: "x"})
}

func (s *mdCaptureServer) Ingest(ctx context.Context, _ *pb.IngestRequest) (*pb.IngestResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.ingst = append(s.ingst, md)
	s.mu.Unlock()
	return &pb.IngestResponse{}, nil
}

func TestGRPCShard_TraceIDReachesShard(t *testing.T) {
	srv := &mdCaptureServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	defer gs.GracefulStop()

	sh, err := NewGRPCShard("trace-shard", lis.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	ctx := weftsdk.ContextWithTraceID(context.Background(), "trace-query-123")
	if _, err := sh.Query(ctx, "SELECT 1"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if err := sh.Ingest(ctx, []*pb.LogRow{{Raw: "r"}}, nil); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.query) != 1 || len(srv.query[0].Get("weft-trace-id")) != 1 || srv.query[0].Get("weft-trace-id")[0] != "trace-query-123" {
		t.Errorf("Query metadata = %v, want weft-trace-id=trace-query-123", srv.query)
	}
	if len(srv.ingst) != 1 || len(srv.ingst[0].Get("weft-trace-id")) != 1 || srv.ingst[0].Get("weft-trace-id")[0] != "trace-query-123" {
		t.Errorf("Ingest metadata = %v, want weft-trace-id=trace-query-123", srv.ingst)
	}
}

// reqCaptureServer records the QueryRequest of every Query.
type reqCaptureServer struct {
	pb.UnimplementedShardQueryServer
	mu   sync.Mutex
	reqs []*pb.QueryRequest
}

func (s *reqCaptureServer) Query(req *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.ResultRow]) error {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return stream.Send(&pb.ResultRow{Raw: "x"})
}

type discardSink struct{}

func (discardSink) SendRow(*pb.ResultRow, string) {}
func (discardSink) Fail(string, error)            {}
func (discardSink) SendSentinel(string)           {}

// QueryFieldsToSink puts the field form on the wire beside sql; QueryToSink
// leaves it off, so a plain query is byte-for-byte what it was.
func TestGRPCShard_QueryFieldsToSinkSendsFieldSQL(t *testing.T) {
	srv := &reqCaptureServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	defer gs.GracefulStop()

	sh, err := NewGRPCShard("field-shard", lis.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	var _ FieldQuerier = sh
	sh.QueryFieldsToSink(context.Background(), "SELECT plain", "SELECT fields", []any{"a"}, discardSink{})
	sh.QueryToSink(context.Background(), "SELECT plain", []any{"a"}, discardSink{})

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.reqs) != 2 {
		t.Fatalf("%d queries, want 2", len(srv.reqs))
	}
	if r := srv.reqs[0]; r.Sql != "SELECT plain" || r.FieldSql != "SELECT fields" || len(r.Args) != 1 {
		t.Errorf("QueryFieldsToSink sent %+v", r)
	}
	if r := srv.reqs[1]; r.Sql != "SELECT plain" || r.FieldSql != "" {
		t.Errorf("QueryToSink sent %+v, want no field_sql", r)
	}
}
