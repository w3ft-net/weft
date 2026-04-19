package shard

import (
	"context"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// GRPCShard implements the Shard interface by connecting to a remote shard
// over gRPC. Drop-in replacement for DuckDBShard in the coordinator.
type GRPCShard struct {
	id     string
	addr   string
	conn   *grpc.ClientConn
	client pb.ShardQueryClient
}

// NewGRPCShard connects to a remote shard gRPC server. token is the
// shared SHARD_SECRET both ends agree on; it's attached as Bearer
// authorization on every outbound call so the shard's
// SharedTokenServerInterceptor accepts the connection. Empty token is
// only valid in tests where the shard also runs without interceptors.
func NewGRPCShard(id, addr, token string) (*GRPCShard, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.ClientTokenUnaryInterceptor(token)),
		grpc.WithStreamInterceptor(auth.ClientTokenStreamInterceptor(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to shard %s at %s: %w", id, addr, err)
	}
	return &GRPCShard{
		id:     id,
		addr:   addr,
		conn:   conn,
		client: pb.NewShardQueryClient(conn),
	}, nil
}

// Configure calls the Configure RPC to assign identity and time window.
func (s *GRPCShard) Configure(ctx context.Context, cfg *pb.ConfigureRequest) error {
	_, err := s.client.Configure(ctx, cfg)
	return err
}

func (s *GRPCShard) ID() string { return s.id }

func (s *GRPCShard) Close() error {
	return s.conn.Close()
}

// Addr returns the gRPC address of this shard.
func (s *GRPCShard) Addr() string { return s.addr }

// Ingest forwards rows to the remote shard for insertion.
func (s *GRPCShard) Ingest(rows []*pb.LogRow) error {
	_, err := s.client.Ingest(context.Background(), &pb.IngestRequest{Rows: rows})
	if err != nil {
		return fmt.Errorf("ingest to shard %s: %w", s.id, err)
	}
	return nil
}

// Query executes SQL on the remote shard and collects all result rows.
func (s *GRPCShard) Query(sql string) ([]*pb.ResultRow, error) {
	stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}

	var rows []*pb.ResultRow
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv from shard %s: %w", s.id, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// QueryToSink executes SQL on the remote shard and streams rows to the sink.
func (s *GRPCShard) QueryToSink(sql string, sink RowSink) {
	defer sink.SendSentinel(s.id)

	stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
	if err != nil {
		log.Printf("shard %s: QueryToSink stream error: %v (sql=%s)", s.id, err, sql)
		return
	}

	for {
		row, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("shard %s: QueryToSink recv error: %v", s.id, err)
			return
		}
		sink.SendRow(row, s.id)
	}
}
