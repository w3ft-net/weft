package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	weftcfg "github.com/kevlarcon/weft/config"
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

	mu             sync.RWMutex
	shard          *shard.DuckDBShard
	id             string
	apps           []string
	heartbeatToken string // per-shard bearer for Heartbeat RPC; populated by Configure or loaded from shard.json

	// dataDir is the directory shard.json lives in. Empty means the
	// server is in test/in-process mode with no persistence.
	dataDir string
	dbPath  string

	// CfgPtr holds the live runtime config. Consulted by Query to pick
	// up the current SHARD_QUERY_TIMEOUT_SECONDS on every request. Nil
	// in tests / in-process mode — runtimeCfg falls through to
	// compile-time defaults.
	CfgPtr *atomic.Pointer[weftcfg.Runtime]
}

func (s *Server) runtimeCfg() *weftcfg.Runtime {
	if s.CfgPtr == nil {
		return weftcfg.Defaults()
	}
	return s.CfgPtr.Load()
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
		shard:          ds,
		id:             id.ShardID,
		apps:           id.Apps,
		heartbeatToken: id.HeartbeatToken,
		dataDir:        dataDir,
		dbPath:         dbPath,
	}, nil
}

// HeartbeatToken returns the per-shard bearer token, or "" if not yet
// christened. Safe to call concurrently from the heartbeat goroutine.
func (s *Server) HeartbeatToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.heartbeatToken
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
		ShardID:        req.ShardId,
		WindowStart:    winStart,
		WindowEnd:      winEnd,
		Apps:           req.Apps,
		HeartbeatToken: req.HeartbeatToken,
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
	s.heartbeatToken = req.HeartbeatToken
	log.Printf("shard configured: id=%s apps=%v window=[%s, %s]",
		req.ShardId, req.Apps, req.WindowStart, req.WindowEnd)

	return &pb.ConfigureResponse{}, nil
}

// Archive exports the shard's data to Parquet on S3. The shard builds the
// full path from the prefix and its own identity, configures DuckDB's httpfs
// from the instance's environment (IAM role + AWS_REGION), and writes directly.
func (s *Server) Archive(ctx context.Context, req *pb.ArchiveRequest) (*pb.ArchiveResponse, error) {
	ds, err := s.getShard()
	if err != nil {
		return nil, err
	}
	path, err := ds.Archive(ctx, req.S3Prefix)
	if err != nil {
		return nil, fmt.Errorf("archive shard %s: %w", s.id, err)
	}
	log.Printf("shard %s: archived to %s", s.id, path)
	return &pb.ArchiveResponse{ParquetPath: path}, nil
}

// Query executes SQL and streams result rows (line_num, raw) to the client.
// Per-request deadline from the runtime config caps pathological queries
// (deep OR chains, raw LIKE '%...%' across billions of rows) — DuckDB
// honors ctx cancellation at row boundaries.
func (s *Server) Query(req *pb.QueryRequest, stream pb.ShardQuery_QueryServer) error {
	ds, err := s.getShard()
	if err != nil {
		return err
	}

	timeoutSec := s.runtimeCfg().ShardQueryTimeoutSeconds
	ctx := stream.Context()
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	rows, err := ds.QueryContext(ctx, req.Sql)
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

// QueryToCompute runs the pushed-down SQL and streams rows straight
// to a compute job on compute_addr. The shard dials compute itself
// (so the coordinator stays off the data path for op 1 of the
// direct-to-compute plan), sends one ComputeMessage per row carrying
// (job_id, this shard's id, row), and finishes with a sentinel so the
// compute-side consumer knows this shard is done. Returns RowsSent so
// the coordinator can fail the query if a shard didn't deliver what
// its pushdown claimed it would.
func (s *Server) QueryToCompute(ctx context.Context, req *pb.QueryToComputeRequest) (*pb.QueryToComputeResponse, error) {
	ds, err := s.getShard()
	if err != nil {
		return nil, err
	}
	if req.ComputeAddr == "" {
		return nil, fmt.Errorf("QueryToCompute: empty compute_addr")
	}
	if req.JobId == "" {
		return nil, fmt.Errorf("QueryToCompute: empty job_id")
	}

	// Honor the same runtime timeout the regular Query RPC does —
	// long-running pushdowns are pathological and we'd rather fail
	// the job than stream forever into compute.
	if timeoutSec := s.runtimeCfg().ShardQueryTimeoutSeconds; timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	rows, err := ds.QueryContext(ctx, req.Sql)
	if err != nil {
		log.Printf("shard %s: QueryToCompute query error: %v (sql=%s)", s.id, err, req.Sql)
		return nil, err
	}

	// Dial compute fresh per request. Connections are cheap over the
	// cluster's internal network and a per-request dial dodges the
	// "compute restarted, our cached channel is stale" failure mode.
	// Compute doesn't enforce shared-token auth (by design — the
	// shard↔compute link is an internal RPC), so bare insecure creds
	// are enough. If that changes, wire ClientTokenUnaryInterceptor
	// with a dedicated secret here.
	conn, err := grpc.NewClient(req.ComputeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("shard %s: dial compute %s: %w", s.id, req.ComputeAddr, err)
	}
	defer conn.Close()

	cc := pb.NewWeftComputeClient(conn)
	stream, err := cc.SendRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("shard %s: open SendRows stream: %w", s.id, err)
	}

	var sent int64
	for _, r := range rows {
		if err := stream.Send(&pb.ComputeMessage{
			JobId:   req.JobId,
			ShardId: s.id,
			Row:     r,
		}); err != nil {
			return nil, fmt.Errorf("shard %s: send row: %w", s.id, err)
		}
		sent++
	}

	// Always close with a sentinel — even on the empty-result path —
	// so compute's per-shard gate unblocks. Without this, one empty
	// shard hangs the whole query.
	if err := stream.Send(&pb.ComputeMessage{
		JobId:    req.JobId,
		ShardId:  s.id,
		Sentinel: true,
	}); err != nil {
		return nil, fmt.Errorf("shard %s: send sentinel: %w", s.id, err)
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return nil, fmt.Errorf("shard %s: close SendRows: %w", s.id, err)
	}

	return &pb.QueryToComputeResponse{RowsSent: sent}, nil
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
