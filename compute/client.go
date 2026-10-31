package compute

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(weftsdk.GRPC.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(weftsdk.GRPC.StreamClientInterceptor()))
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
		if resp.MissingShard != "" {
			return Line{}, false, &MissingRowsError{ShardID: resp.MissingShard, Err: errors.New(resp.Error)}
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
