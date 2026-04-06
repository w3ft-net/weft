package service

import (
	"context"
	"io"
	"log"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// Server implements the ShardQuery gRPC service backed by a DuckDBShard.
type Server struct {
	pb.UnimplementedShardQueryServer
	shard *shard.DuckDBShard
}

// NewServer creates a new shard gRPC server.
func NewServer(s *shard.DuckDBShard) *Server {
	return &Server{shard: s}
}

// Query executes SQL and streams matching rows to the client.
func (s *Server) Query(req *pb.QueryRequest, stream pb.ShardQuery_QueryServer) error {
	rows, err := s.shard.Query(req.Sql)
	if err != nil {
		return err
	}

	for _, r := range rows {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}

// QueryToCompute is a placeholder — in Phase 1 the coordinator manages
// shard→compute streaming. In Phase 2 the shard would connect directly
// to the compute service.
func (s *Server) QueryToCompute(_ context.Context, req *pb.QueryToComputeRequest) (*pb.QueryToComputeResponse, error) {
	return &pb.QueryToComputeResponse{}, nil
}

// Ingest accepts a batch of rows for insertion.
func (s *Server) Ingest(_ context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	if err := s.shard.Ingest(req.Rows); err != nil {
		return nil, err
	}
	return &pb.IngestResponse{RowsIngested: int64(len(req.Rows))}, nil
}

// IngestStream accepts a long-lived stream of LogRows from a weftd agent.
func (s *Server) IngestStream(stream pb.ShardQuery_IngestStreamServer) error {
	var rows []*pb.LogRow
	var count int64

	for {
		row, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		rows = append(rows, row)
		count++

		if len(rows) >= 100 {
			if err := s.shard.Ingest(rows); err != nil {
				log.Printf("ingest-stream: %v", err)
			}
			rows = rows[:0]
		}
	}

	if len(rows) > 0 {
		if err := s.shard.Ingest(rows); err != nil {
			log.Printf("ingest-stream flush: %v", err)
		}
	}

	log.Printf("ingest-stream: received %d rows", count)
	return stream.SendAndClose(&pb.IngestStreamResponse{RowsReceived: count})
}
