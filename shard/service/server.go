package service

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	weftcfg "github.com/graybeardcorp/weft/config"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
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

	// Volume is the data volume, unmounted by Pause and remounted by Resume
	// or a spare's Activate. Nil (dev, container mode) skips both.
	Volume Volume

	// The gate (gate.go): gateMu guards mode, successor, inflight, queries,
	// spare and released. It is separate from mu so admitting a call never
	// waits on a database open.
	gateMu    sync.Mutex
	mode      serveMode
	successor string // the address paused refusals redirect to
	inflight  int
	queries   map[*gatedQuery]struct{} // admitted queries, which Pause may cut
	spare     bool                     // a warm-pool instance not yet given a volume
	released  bool                     // the volume is not mounted (a spare, or after Pause)
	shardOpts []shard.Option           // how every database this server opens is configured

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
func NewBlankServer(dataDir, dbPath string, opts ...shard.Option) *Server {
	return &Server{dataDir: dataDir, dbPath: dbPath, shardOpts: opts}
}

// NewServerFromIdentity reads shard.json from dataDir, opens the DuckDB
// at dbPath, and returns a configured Server. Use when the shard restarts
// against an existing EBS volume.
func NewServerFromIdentity(dataDir, dbPath string, opts ...shard.Option) (*Server, error) {
	id, err := shard.LoadIdentity(dataDir)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	if id == nil {
		return nil, fmt.Errorf("no shard.json in %s", dataDir)
	}
	ds, err := shard.NewDuckDBShard(id.ShardID, dbPath, opts...)
	if err != nil {
		return nil, fmt.Errorf("open duckdb at %s: %w", dbPath, err)
	}
	return &Server{
		shardOpts:      opts,
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
func (s *Server) Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shard != nil {
		// Idempotent for the same id: the coordinator re-dials and
		// re-Configures on every promote/reconnect (e.g. a shard still
		// configured from before a coordinator restart). Returning an error
		// there wedges the shard out of promotion. A DIFFERENT id is still
		// refused — a shard's identity is for life.
		if s.id == req.ShardId {
			return &pb.ConfigureResponse{}, nil
		}
		return nil, fmt.Errorf("shard already configured as %s, refusing to reconfigure as %s", s.id, req.ShardId)
	}

	if s.dataDir == "" {
		// In-process test mode: no persistence, in-memory DB.
		ds, err := shard.NewDuckDBShard(req.ShardId, "", s.shardOpts...)
		if err != nil {
			return nil, fmt.Errorf("create shard: %w", err)
		}
		s.shard = ds
		s.id = req.ShardId
		s.apps = req.Services
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
		Apps:           req.Services,
		HeartbeatToken: req.HeartbeatToken,
		Layout:         shard.LayoutFromProto(req.Layout),
	}
	if err := shard.SaveIdentity(s.dataDir, ident); err != nil {
		return nil, fmt.Errorf("persist identity: %w", err)
	}

	ds, err := shard.NewDuckDBShard(req.ShardId, s.dbPath, s.shardOpts...)
	if err != nil {
		return nil, fmt.Errorf("open duckdb at %s: %w", s.dbPath, err)
	}
	s.shard = ds
	s.id = req.ShardId
	s.apps = req.Services
	s.heartbeatToken = req.HeartbeatToken
	weftsdk.Infof(ctx, "shard configured: id=%s apps=%v window=[%s, %s]",
		req.ShardId, req.Services, req.WindowStart, req.WindowEnd)

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
	path, stats, err := ds.Archive(ctx, req.S3Prefix)
	if err != nil {
		return nil, fmt.Errorf("archive shard %s: %w", s.id, err)
	}
	weftsdk.Infof(ctx, "shard %s: archived to %s", s.id, path)
	return &pb.ArchiveResponse{
		ParquetPath:   path,
		CustomerStats: shard.CustomerStatsToProto(stats),
	}, nil
}

// Query executes SQL and streams result rows (raw, ts) to the client.
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

	// "shard.Query" bookends this call with weftsdk's own "span start"/
	// "span done" lines when traced (no-op otherwise). defer span.End()
	// alone, unconditionally — RecordError below marks the eventual
	// done line's status at the point an error is actually discovered,
	// covering every return path with no end-of-function branching.
	// Unlike cold-shard queries (always logged — see shard/cold_shard.go),
	// live-shard queries stay silent unless actively traced: this is the
	// hot, frequent path (nearly every query touches a live shard), so
	// unconditional per-query logging here would be real log spam in a
	// way cold-shard logging isn't. The sql= line below is this shard's
	// own additional detail, kept alongside the generic span bookend
	// rather than replaced by it.
	ctx, span := weftsdk.StartSpan(ctx, "shard.Query")
	defer span.End()
	if _, ok := weftsdk.TraceIDFromContext(ctx); ok {
		weftsdk.Infof(ctx, "shard %s: query (sql=%s)", s.id, req.Sql)
	}

	sql := req.Sql
	if req.FieldSql != "" {
		sql = req.FieldSql
	}
	rows, err := ds.QueryContextTable(ctx, req.Table, sql, stringArgsToAny(req.Args)...)
	if err != nil {
		span.RecordError(err)
		weftsdk.Errorf(ctx, "shard %s: query error: %v (sql=%s args=%v)", s.id, err, sql, req.Args)
		return err
	}

	for _, r := range rows {
		if err := stream.Send(r); err != nil {
			span.RecordError(err)
			return err
		}
	}
	return nil
}

// stringArgsToAny widens the on-wire []string back to the []any shape
// DuckDB's driver expects. Empty in → nil out so callers without
// placeholders use a 0-arg QueryContext.
func stringArgsToAny(args []string) []any {
	if len(args) == 0 {
		return nil
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a
	}
	return out
}

// Ingest accepts a batch of rows for insertion.
func (s *Server) Ingest(ctx context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	ds, err := s.getShard()
	if err != nil {
		return nil, err
	}
	if err := ds.Ingest(ctx, req.Rows, nil); err != nil {
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
			if err := ds.Ingest(stream.Context(), rows, nil); err != nil {
				weftsdk.Infof(stream.Context(), "ingest-stream: %v", err)
			}
			rows = rows[:0]
		}
	}

	if len(rows) > 0 {
		if err := ds.Ingest(stream.Context(), rows, nil); err != nil {
			weftsdk.Infof(stream.Context(), "ingest-stream flush: %v", err)
		}
	}

	weftsdk.Infof(stream.Context(), "ingest-stream: received %d rows", count)
	return stream.SendAndClose(&pb.IngestStreamResponse{RowsReceived: count})
}
