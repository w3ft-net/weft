package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// Server implements the ShardQuery gRPC service backed by a DuckDBShard.
// Two startup modes:
//   - NewServerFromIdentity: EBS volume already carries shard.json; open
//     DuckDB with that identity at startup.
//   - NewBlankServer: fresh EBS, no identity on disk. The shard reports
//     as unclaimed in heartbeats until the coordinator christens it via
//     Configure, at which point we write shard.json and open DuckDB.
//
// Tests may still pass an already-opened *DuckDBShard via NewServer; that
// path is identity-less and used only in in-process unit tests.
type Server struct {
	pb.UnimplementedShardQueryServer

	mu    sync.RWMutex
	shard *shard.DuckDBShard
	id    string
	apps  []string

	// dataDir is the directory shard.json lives in. Empty means the
	// server is in test/in-process mode with no persistence.
	dataDir string
	dbPath  string
}

// NewServer wraps an already-open DuckDBShard. Used by in-process tests
// where no identity file is needed.
func NewServer(s *shard.DuckDBShard) *Server {
	srv := &Server{shard: s}
	if s != nil {
		srv.id = s.ID()
	}
	return srv
}

// NewBlankServer creates a Server for a fresh EBS volume — no identity
// yet. Calls to Query/Ingest fail until Configure is invoked. dataDir is
// where shard.json gets written; dbPath is where the DuckDB file will be
// created on Configure.
func NewBlankServer(dataDir, dbPath string) *Server {
	return &Server{dataDir: dataDir, dbPath: dbPath}
}

// NewServerFromIdentity reads shard.json from dataDir, opens the DuckDB
// at dbPath, and returns a configured Server. Use when the shard restarts
// against an existing EBS volume.
func NewServerFromIdentity(dataDir, dbPath string) (*Server, error) {
	id, err := shard.LoadIdentity(dataDir)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	if id == nil {
		return nil, fmt.Errorf("no shard.json in %s", dataDir)
	}
	ds, err := shard.NewDuckDBShard(id.ShardID, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open duckdb at %s: %w", dbPath, err)
	}
	return &Server{
		shard:   ds,
		id:      id.ShardID,
		apps:    id.Apps,
		dataDir: dataDir,
		dbPath:  dbPath,
	}, nil
}

// Identity returns the current shard id, or "" when unclaimed. Safe to
// call from any goroutine (e.g. the heartbeat loop in cmd/shard/main.go).
func (s *Server) Identity() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

// Shard returns the underlying DuckDBShard, or nil if the server hasn't
// been configured yet. The heartbeat loop uses this to report row count.
func (s *Server) Shard() *shard.DuckDBShard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shard
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

// Configure assigns an identity, opens the DuckDB, and persists the
// identity to disk. Rejects reconfigure: once a shard has an id, it
// keeps that id for life.
func (s *Server) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shard != nil {
		return nil, fmt.Errorf("shard already configured as %s", s.id)
	}

	if s.dataDir == "" {
		// In-process test mode: no persistence, in-memory DB.
		ds, err := shard.NewDuckDBShard(req.ShardId, "")
		if err != nil {
			return nil, fmt.Errorf("create shard: %w", err)
		}
		s.shard = ds
		s.id = req.ShardId
		s.apps = req.Apps
		return &pb.ConfigureResponse{}, nil
	}

	// Production path: persist identity first, then open DuckDB.
	// Do it in this order so a crash between the two leaves a readable
	// identity on disk and the next restart picks up right where we left
	// off — NewServerFromIdentity will open the DuckDB fresh.
	winStart, _ := time.Parse(time.RFC3339, req.WindowStart)
	winEnd, _ := time.Parse(time.RFC3339, req.WindowEnd)
	ident := &shard.Identity{
		ShardID:     req.ShardId,
		WindowStart: winStart,
		WindowEnd:   winEnd,
		Apps:        req.Apps,
	}
	if err := shard.SaveIdentity(s.dataDir, ident); err != nil {
		return nil, fmt.Errorf("persist identity: %w", err)
	}

	ds, err := shard.NewDuckDBShard(req.ShardId, s.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open duckdb at %s: %w", s.dbPath, err)
	}
	s.shard = ds
	s.id = req.ShardId
	s.apps = req.Apps
	log.Printf("shard configured: id=%s apps=%v window=[%s, %s]",
		req.ShardId, req.Apps, req.WindowStart, req.WindowEnd)

	return &pb.ConfigureResponse{}, nil
}

// Query executes SQL and streams result rows (line_num, raw) to the client.
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
