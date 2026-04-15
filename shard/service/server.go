package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// Server implements the ShardQuery gRPC service backed by a DuckDBShard.
// The shard can be nil at startup (blank-start mode) and initialized
// later via the Configure RPC.
type Server struct {
	pb.UnimplementedShardQueryServer
	mu    sync.RWMutex
	shard *shard.DuckDBShard
	id    string
	apps  []string
}

// NewServer creates a new shard gRPC server. s can be nil for blank-start mode.
func NewServer(s *shard.DuckDBShard) *Server {
	srv := &Server{shard: s}
	if s != nil {
		srv.id = s.ID()
	}
	return srv
}

// getShard returns the shard, or an error if not yet configured.
func (s *Server) getShard() (*shard.DuckDBShard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.shard == nil {
		return nil, fmt.Errorf("shard not configured — waiting for Configure RPC")
	}
	return s.shard, nil
}

// Configure assigns an identity and creates the DuckDB for a blank shard.
func (s *Server) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shard != nil {
		return nil, fmt.Errorf("shard already configured as %s", s.id)
	}

	ds, err := shard.NewDuckDBShard(req.ShardId, "")
	if err != nil {
		return nil, fmt.Errorf("create shard: %w", err)
	}

	s.shard = ds
	s.id = req.ShardId
	s.apps = req.Apps
	log.Printf("shard configured: id=%s apps=%v window=[%s, %s]",
		req.ShardId, req.Apps, req.WindowStart, req.WindowEnd)

	return &pb.ConfigureResponse{}, nil
}

// Query executes SQL and streams matching rows to the client.
func (s *Server) Query(req *pb.QueryRequest, stream pb.ShardQuery_QueryServer) error {
	ds, err := s.getShard()
	if err != nil {
		return err
	}
	rows, err := ds.Query(req.Sql)
	if err != nil {
		log.Printf("shard %s: query error: %v (sql=%s)", s.id, err, req.Sql)
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
	ds, err := s.getShard()
	if err != nil {
		return nil, err
	}
	if err := ds.Ingest(req.Rows); err != nil {
		return nil, err
	}
	return &pb.IngestResponse{RowsIngested: int64(len(req.Rows))}, nil
}

// IngestStream accepts a long-lived stream of LogRows from a weftd agent.
func (s *Server) IngestStream(stream pb.ShardQuery_IngestStreamServer) error {
	ds, err := s.getShard()
	if err != nil {
		return err
	}

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
			if err := ds.Ingest(rows); err != nil {
				log.Printf("ingest-stream: %v", err)
			}
			rows = rows[:0]
		}
	}

	if len(rows) > 0 {
		if err := ds.Ingest(rows); err != nil {
			log.Printf("ingest-stream flush: %v", err)
		}
	}

	log.Printf("ingest-stream: received %d rows", count)
	return stream.SendAndClose(&pb.IngestStreamResponse{RowsReceived: count})
}
