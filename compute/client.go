package compute

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"github.com/w3ft-net/weft/transport"
	"io"

	"google.golang.org/grpc"

	pb "github.com/w3ft-net/weft/proto/weftpb"
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
		transport.DialOption(),
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
	// scanned is the lines the query's cold shards scanned, from the last
	// message that carried it.
	scanned int64
}

// RowsScanned implements ScanCounter. It is complete once Next has reported
// the end of the stream.
func (s *grpcLineStream) RowsScanned() int64 { return s.scanned }

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
		if resp.RowsScanned != 0 {
			s.scanned = resp.RowsScanned
		}
		s.buf = resp.Lines
		s.idx = 0
	}
	line := Line{Raw: s.buf[s.idx]}
	s.idx++
	return line, true, nil
}

func (s *grpcLineStream) Close() error { return nil }
