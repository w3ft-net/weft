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
