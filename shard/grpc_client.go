package shard

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// Retry budget for transient shard-side failures. See
// docs/plans/shard-grpc-resilience.md for the failure mode
// (rebind-mid-flight after a coordinator restart or an ASG
// instance replacement). 3 attempts × 100ms ≈ 200ms worst-case
// added to user-visible tail latency, which matters less than
// the alternative of returning `Canceled` to the user mid-deploy.
const (
	maxRetryAttempts = 3
	retryBackoff     = 100 * time.Millisecond
)

// isRetriable narrows the retry class to gRPC connection-lifecycle
// errors. Canceled appears when the underlying *grpc.ClientConn is
// closed mid-RPC; Unavailable when the new conn after a rebind isn't
// ready yet. Deliberately leaves InvalidArgument / NotFound /
// PermissionDenied / etc. alone so the caller sees the real error
// without a 200ms delay.
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.Canceled || s.Code() == codes.Unavailable
	}
	return false
}

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

func (s *GRPCShard) Archive(ctx context.Context, s3Prefix string) (string, error) {
	resp, err := s.client.Archive(ctx, &pb.ArchiveRequest{S3Prefix: s3Prefix})
	if err != nil {
		return "", fmt.Errorf("archive shard %s: %w", s.id, err)
	}
	return resp.ParquetPath, nil
}

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

// Query executes SQL on the remote shard and collects all result
// rows. Retries the entire attempt on Canceled/Unavailable up to
// maxRetryAttempts times — the buffered slice is local, so a failed
// mid-stream attempt is safely discarded and re-driven from scratch.
func (s *GRPCShard) Query(sql string, args ...any) ([]*pb.ResultRow, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetryAttempts; attempt++ {
		rows, err := s.queryOnce(sql, args)
		if err == nil {
			return rows, nil
		}
		if !isRetriable(err) {
			return nil, err
		}
		lastErr = err
		if attempt < maxRetryAttempts-1 {
			log.Printf("shard %s: Query attempt %d failed (%v); retrying", s.id, attempt+1, err)
			time.Sleep(retryBackoff)
		}
	}
	return nil, fmt.Errorf("shard %s: gave up after %d attempts: %w", s.id, maxRetryAttempts, lastErr)
}

func (s *GRPCShard) queryOnce(sql string, args []any) ([]*pb.ResultRow, error) {
	stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql, Args: anyArgsToStrings(args)})
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

// QueryToSink executes SQL on the remote shard and streams rows to
// the sink. Retries the *pre-first-row* failure window: setup errors
// and a first-Recv-with-no-rows-yet are safe to retry because the
// sink hasn't been written to. Once any row reaches the sink, a
// retry would double-send, so a mid-stream failure logs and bails.
func (s *GRPCShard) QueryToSink(sql string, args []any, sink RowSink) {
	defer sink.SendSentinel(s.id)

	var lastErr error
	for attempt := 0; attempt < maxRetryAttempts; attempt++ {
		rowsSent, err := s.queryToSinkOnce(sql, args, sink)
		if err == nil {
			return
		}
		if rowsSent > 0 {
			// Sink saw partial output; retrying would double-send.
			log.Printf("shard %s: QueryToSink mid-stream error after %d rows: %v", s.id, rowsSent, err)
			return
		}
		if !isRetriable(err) {
			log.Printf("shard %s: QueryToSink failed: %v (sql=%s)", s.id, err, sql)
			return
		}
		lastErr = err
		if attempt < maxRetryAttempts-1 {
			log.Printf("shard %s: QueryToSink attempt %d failed pre-row (%v); retrying", s.id, attempt+1, err)
			time.Sleep(retryBackoff)
		}
	}
	log.Printf("shard %s: QueryToSink gave up after %d attempts: %v (sql=%s)", s.id, maxRetryAttempts, lastErr, sql)
}

// queryToSinkOnce attempts a single QueryToSink streaming pass.
// Returns (rowsSent, err); err==nil means EOF after streaming
// completed. A non-nil err with rowsSent==0 is safe to retry; with
// rowsSent>0 it isn't.
func (s *GRPCShard) queryToSinkOnce(sql string, args []any, sink RowSink) (int, error) {
	stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql, Args: anyArgsToStrings(args)})
	if err != nil {
		return 0, err
	}
	rowsSent := 0
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			return rowsSent, nil
		}
		if err != nil {
			return rowsSent, err
		}
		sink.SendRow(row, s.id)
		rowsSent++
	}
}
