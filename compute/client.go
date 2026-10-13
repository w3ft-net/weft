package compute

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	weftsdk "github.com/w3ft-net/weft-go"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// Client implements Executor by connecting to a remote compute
// worker over gRPC. The worker runs an Engine (cmd/compute).
type Client struct {
	Addr   string
	conn   *grpc.ClientConn
	client pb.WeftComputeClient
}

// NewClient connects to a remote compute worker.
func NewClient(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to compute at %s: %w", addr, err)
	}
	return &Client{
		Addr:   addr,
		conn:   conn,
		client: pb.NewWeftComputeClient(conn),
	}, nil
}

// Close closes the gRPC connection.
func (p *Client) Close() error {
	return p.conn.Close()
}

func (p *Client) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (LineStream, error) {
	// Trace courier (out to compute): context.Value doesn't cross a gRPC
	// call on its own — ferry the ambient trace ID as `weft-trace-id`
	// metadata, mirroring the api→coordinator boundary (coordCtx in
	// api/handlers.go) and the pre-existing weft-dirstack pattern. The
	// ambient span ID (coordinator's own "coordinator.Execute" span, if
	// StartSpan established one) rides alongside as `weft-span-id` so
	// the span compute starts on the other end parents off it.
	if traceID, ok := weftsdk.TraceIDFromContext(ctx); ok {
		ctx = metadata.AppendToOutgoingContext(ctx, "weft-trace-id", traceID)
	}
	if spanID, ok := weftsdk.CurrentSpanID(ctx); ok {
		ctx = metadata.AppendToOutgoingContext(ctx, "weft-span-id", spanID)
	}
	stream, err := p.client.ExecuteQueryStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("execute query stream: %w", err)
	}
	return &grpcLineStream{stream: stream}, nil
}

// grpcLineStream adapts a gRPC server-stream into a compute.LineStream.
type grpcLineStream struct {
	stream pb.WeftCompute_ExecuteQueryStreamClient
	buf    []string
	idx    int
}

func (s *grpcLineStream) Next(_ context.Context) (Line, bool, error) {
	for s.idx >= len(s.buf) {
		resp, err := s.stream.Recv()
		if err != nil {
			if err == io.EOF {
				return Line{}, false, nil
			}
			return Line{}, false, fmt.Errorf("compute stream: %w", err)
		}
		if resp.Error != "" {
			return Line{}, false, fmt.Errorf("compute error: %s", resp.Error)
		}
		s.buf = resp.Lines
		s.idx = 0
	}
	line := Line{Raw: s.buf[s.idx]}
	s.idx++
	return line, true, nil
}

func (s *grpcLineStream) Close() error { return nil }
