package logdb_test

// A genuine integration test for the FSM launch→promote path. Unlike the
// white-box coordination test (package logdb), this:
//
//   - uses ONLY exported logdb API (NewProvisioner, SetStore,
//     LoadStateMachines, Start, ProcessHeartbeat) — no unexported access,
//     no private-field injection, no choosing the connection model;
//   - runs against a real, pooled store (store.Open) on a private
//     throwaway database, so launches happen with genuine concurrency
//     (the prod connection pool), not serialized around one tx connection;
//   - dials a REAL gRPC shard server for Configure (the production
//     grpcConfigurer, not a stub), so promotion exercises the real wire.
//
// Isolation: it provisions its own database via the postgres maintenance
// DB and drops it in cleanup, so it neither collides with the live
// coordinator (the integration/ package's shared stack) nor depends on
// the shared test DB's seeded state.
//
// What's still not "the real world": the shards are in-process gRPC
// servers rather than EC2/docker instances, and heartbeats are delivered
// by calling the exported ProcessHeartbeat directly rather than over the
// Heartbeat RPC (whose handler just authenticates and delegates to it).
// The full container e2e belongs in the docker-compose integration suite.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	shardsvc "github.com/kevlarcon/weft/shard/service"
	"github.com/kevlarcon/weft/store"
)

// realShardLauncher satisfies logdb.ShardLauncher (constructor) and
// logdb.ShardLaunchAPI (FSM). Each Launch starts a real in-process gRPC
// shard server and a heartbeat goroutine that reports to the provisioner
// via ProcessHeartbeat — the same loop a real shard runs. No gate: the
// pooled store handles the concurrent launches and heartbeats.
type realShardLauncher struct {
	prov *logdb.Provisioner

	mu     sync.Mutex
	seq    int
	shards map[string]*inProcShard // instanceID → running shard
}

type inProcShard struct {
	svc  *shardsvc.Server
	gs   *grpc.Server
	addr string
	stop chan struct{}
}

func newRealShardLauncher() *realShardLauncher {
	return &realShardLauncher{shards: map[string]*inProcShard{}}
}

func (l *realShardLauncher) Launch(_ context.Context, spec logdb.LaunchSpec) (logdb.LaunchResult, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return logdb.LaunchResult{}, err
	}
	svc := shardsvc.NewServer(nil) // unconfigured — Configure (over gRPC) assigns identity + opens an in-mem DuckDB
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, svc)
	go gs.Serve(lis)

	l.mu.Lock()
	l.seq++
	instanceID := fmt.Sprintf("i-%d", l.seq)
	sh := &inProcShard{svc: svc, gs: gs, addr: lis.Addr().String(), stop: make(chan struct{})}
	l.shards[instanceID] = sh
	l.mu.Unlock()

	// Heartbeat loop: empty ShardID until Configure assigns identity, then
	// the real id — exactly what cmd/shard does.
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sh.stop:
				return
			case <-ticker.C:
				id := svc.Identity()
				state := "ADOPTED"
				if id != "" {
					state = "INGESTING"
				}
				var rows int64
				if ds := svc.Shard(); ds != nil {
					rows = ds.RowCount()
				}
				l.prov.ProcessHeartbeat(store.ShardHeartbeat{
					ShardID:    id,
					InstanceID: instanceID,
					Addr:       sh.addr,
					State:      state,
					RowCount:   rows,
				})
			}
		}
	}()

	vol := spec.ExistingVolumeID
	if vol == "" {
		vol = "vol-" + instanceID
	}
	return logdb.LaunchResult{InstanceID: instanceID, VolumeID: vol, AZ: spec.AZ}, nil
}

func (l *realShardLauncher) Terminate(_ context.Context, instanceID string) error {
	l.mu.Lock()
	sh := l.shards[instanceID]
	delete(l.shards, instanceID)
	l.mu.Unlock()
	if sh != nil {
		close(sh.stop)
		sh.gs.Stop()
	}
	return nil
}

func (l *realShardLauncher) LaunchInstance(context.Context, string) (string, error) {
	return "", fmt.Errorf("realShardLauncher: LaunchInstance is not the FSM path")
}
func (l *realShardLauncher) TerminateInstance(context.Context, string, string) error { return nil }

func (l *realShardLauncher) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, sh := range l.shards {
		close(sh.stop)
		sh.gs.Stop()
	}
	l.shards = map[string]*inProcShard{}
}

// TestIntegration_BucketScaleUpLaunchPromote stands up a real pooled
// provisioner over a private DB and asserts that a Target=2 bucket drives
// two real in-process shards all the way to INGESTING through genuinely
// concurrent launches + real gRPC Configure — no gating, no fakes.
func TestIntegration_BucketScaleUpLaunchPromote(t *testing.T) {
	db, dropDB := openPrivateStore(t)
	defer dropDB()

	// Neutralize the migration-seeded default bucket so only our bucket
	// scales (Target<=0 means the bucket SM manages nothing).
	if def, ok, err := db.GetBucketByLabel("default"); err == nil && ok {
		def.TargetIngestingCount = 0
		if err := db.SaveBucket(&def); err != nil {
			t.Fatalf("zero default bucket: %v", err)
		}
	}
	bucket := &store.Bucket{
		Label:                "fsmint",
		Services:             []string{},
		TargetIngestingCount: 2,
		Tier:                 "small",
		WindowDuration:       (24 * time.Hour).String(),
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
	}
	if err := db.SaveBucket(bucket); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	launcher := newRealShardLauncher()
	reg := logdb.NewShardRegistry()
	reg.SetStore(logdb.ShardStore{Store: db})
	prov := logdb.NewProvisioner(reg, launcher, logdb.ProvisionerConfig{
		PollInterval:   100 * time.Millisecond,
		WindowDuration: 24 * time.Hour,
		// ShardSecret "" → grpcConfigurer dials with no bearer, matching
		// the in-process server's no-interceptor setup.
	})
	launcher.prov = prov
	prov.SetStore(db)
	prov.LoadStateMachines()
	prov.Start(ctx)

	t.Cleanup(func() {
		cancel()
		launcher.closeAll()
		time.Sleep(150 * time.Millisecond) // let in-flight ticks/heartbeats drain before the pool closes
	})

	// The provisioner's own ticker mints two shards, launches them
	// concurrently, and the heartbeat loops promote them. Poll for the
	// steady state.
	deadline := time.Now().Add(15 * time.Second)
	for prov.TotalIngesting() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: expected 2 INGESTING shards, got %d", prov.TotalIngesting())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Confirm against the store, scoped to our bucket: exactly two
	// INGESTING shard rows.
	recs, err := (logdb.ShardStore{Store: db}).ListShards()
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	ingesting := 0
	for _, r := range recs {
		if r.BucketID == bucket.ID && r.State == logdb.StateIngesting {
			ingesting++
		}
	}
	if ingesting != 2 {
		t.Fatalf("store shows %d INGESTING shards for bucket %d, want 2", ingesting, bucket.ID)
	}
}

// openPrivateStore creates a throwaway database off the test postgres,
// opens a pooled store against it (running migrations), and returns the
// store plus a drop function. Skips if postgres is unreachable.
func openPrivateStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	base := os.Getenv("WEFT_TEST_DSN")
	if base == "" {
		base = "host=localhost port=5433 dbname=weft_test user=weft password=weft sslmode=disable"
	}
	maintenance := withDBName(base, "postgres")
	dbName := fmt.Sprintf("weft_fsmint_%d", time.Now().UnixNano())

	admin, err := sql.Open("pgx", maintenance)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("postgres unavailable: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + quoteIdent(dbName)); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", dbName, err)
	}
	admin.Close()

	// The freshly-created DB has no goose_db_version table; allow Migrate
	// to bootstrap schema.sql (the dev/CI fixture path).
	t.Setenv("WEFT_ALLOW_SCHEMA_BOOTSTRAP", "1")
	db, err := store.Open(withDBName(base, dbName))
	if err != nil {
		t.Fatalf("open private store: %v", err)
	}

	drop := func() {
		_ = db.Close()
		a, err := sql.Open("pgx", maintenance)
		if err != nil {
			return
		}
		defer a.Close()
		// FORCE (pg 13+) terminates any lingering backends so the drop
		// doesn't hang on a connection the pool hadn't closed yet.
		if _, err := a.Exec("DROP DATABASE IF EXISTS " + quoteIdent(dbName) + " WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v (leftover test DB)", dbName, err)
		}
	}
	return db, drop
}

// withDBName swaps the dbname in a libpq-style space-separated DSN.
func withDBName(dsn, name string) string {
	parts := strings.Fields(dsn)
	for i, p := range parts {
		if strings.HasPrefix(p, "dbname=") {
			parts[i] = "dbname=" + name
			return strings.Join(parts, " ")
		}
	}
	return strings.Join(append(parts, "dbname="+name), " ")
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
