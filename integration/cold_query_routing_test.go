//go:build integration && !localstack

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardservice "github.com/kevlarcon/weft/shard/service"
	"github.com/kevlarcon/weft/store"
)

func TestColdShardQueryRouting(t *testing.T) {
	s3cfg := shard.S3ConfigFromEnv()
	if s3cfg == nil {
		t.Fatal("S3 config not set — harness should have configured env vars")
	}

	db := openTestStore(t)
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	cust, err := tx.FindOrCreateCustomer("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.FindOrCreateNamespace(cust.ID, "vimgym"); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateSubscription(&store.Subscription{
		ID:         "cold-query-bolt",
		CustomerID: "kevlar",
		Tier:       string(store.TierBolt),
		Status:     "active",
		StartsAt:   time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Two time windows: shard-0 covers [t0, t1), shard-1 covers [t1, t2).
	t0 := time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)

	s0, err := shard.NewDuckDBShard("cold-route-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s0.Close() })

	s1, err := shard.NewDuckDBShard("cold-route-1", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s0.Ingest([]*pb.LogRow{
		{LineNum: 1, Raw: "hot line 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(time.Minute).UnixNano()},
		{LineNum: 2, Raw: "hot line 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Minute).UnixNano()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Ingest([]*pb.LogRow{
		{LineNum: 3, Raw: "cold line 3", App: "vimgym", Level: "WARN", Customer: "kevlar", TsUnixNs: t1.Add(time.Minute).UnixNano()},
		{LineNum: 4, Raw: "cold line 4", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t1.Add(2 * time.Minute).UnixNano()},
	}); err != nil {
		t.Fatal(err)
	}

	// Start shard-0 as a gRPC server so the compute pool can dial it.
	s0Addr := startShardGRPC(t, s0)

	// Export shard-1 to MinIO, then serve a test Lambda handler so the
	// compute pool can query archived data via HTTP (same path as the
	// real Lambda cold-shard function).
	s3Path := fmt.Sprintf("s3://%s/test/cold-route-1.parquet", ColdBucket)
	if err := s1.ExportToS3Parquet(s3Path, s3cfg); err != nil {
		t.Fatalf("export to MinIO: %v", err)
	}
	s1.Close()

	lambdaSrv := startColdShardHTTP(t, s3cfg)

	// The registry filter requires Shard != nil for entries without an
	// Addr (line 450 of registry.go). In production the provisioner sets
	// entry.Shard to a ColdShard; replicate that here. The compute pool
	// dials its own shard via DialShard, so this ColdShard is only used
	// to pass the filter.
	cold, err := shard.NewColdShard("cold-route-1", []string{"vimgym"}, s3Path, lambdaSrv.URL, nil)
	if err != nil {
		t.Fatalf("create cold shard: %v", err)
	}
	t.Cleanup(func() { cold.Close() })

	apps := []string{"vimgym"}
	c := coordinator.NewCoordinator(coordinator.CoordinatorConfig{
		Store:             tx,
		LambdaFunctionURL: lambdaSrv.URL,
	})
	c.Registry.Add(&coordinator.ShardEntry{
		ID: "cold-route-0", Apps: apps,
		WindowStart: t0, WindowEnd: t1,
		State: coordinator.StateIngesting, Addr: s0Addr,
		CreatedAt: time.Now(),
	})
	c.Registry.Add(&coordinator.ShardEntry{
		ID: "cold-route-1", Apps: apps,
		WindowStart: t1, WindowEnd: t2,
		State: coordinator.StateArchived, ParquetPath: s3Path,
		Shard: cold,
		CreatedAt: time.Now(),
	})

	// --- Query each window independently, then combined ---

	hotWindow := coordinator.TimeRange{Start: t0, End: t1}
	coldWindow := coordinator.TimeRange{Start: t1, End: t2}
	fullWindow := coordinator.TimeRange{Start: t0, End: t2}

	hotRows := drainQuery(t, c, `cat vimgym.log`, hotWindow)
	if len(hotRows) == 0 {
		t.Fatal("hot shard: no rows returned")
	}

	coldRows := drainQuery(t, c, `cat vimgym.log`, coldWindow)
	if len(coldRows) == 0 {
		t.Fatal("cold/archived shard: no rows returned — archived shard not participating in query routing")
	}

	fullRows := drainQuery(t, c, `cat vimgym.log`, fullWindow)
	if len(fullRows) < len(hotRows)+len(coldRows) {
		t.Errorf("full range returned %d rows, want >= %d+%d from hot+cold",
			len(fullRows), len(hotRows), len(coldRows))
	}

	t.Logf("hot=%d, cold=%d, combined=%d", len(hotRows), len(coldRows), len(fullRows))

	// Regression: a stale LastHeartbeatAt must not hide an archived shard.
	// Before the fix, archive() didn't zero LastHeartbeatAt, so after 2min
	// the cold shard became invisible to MatchingShards.
	entry1 := c.Registry.Get("cold-route-1")
	entry1.LastHeartbeatAt = time.Now().Add(-5 * time.Minute)
	staleRows := drainQuery(t, c, `cat vimgym.log`, coldWindow)
	if len(staleRows) != 0 {
		t.Logf("regression confirmed: stale heartbeat on archived shard still returns %d rows (isStale would filter in production)", len(staleRows))
	}
	// The provisioner fix zeros LastHeartbeatAt on archive. Verify that
	// zeroing restores visibility.
	entry1.LastHeartbeatAt = time.Time{}
	restoredRows := drainQuery(t, c, `cat vimgym.log`, coldWindow)
	if len(restoredRows) == 0 {
		t.Fatal("archived shard invisible after zeroing LastHeartbeatAt — isStale check broken")
	}
}

// startShardGRPC wraps a DuckDBShard in a gRPC server and returns the
// listener address. The server is stopped when the test ends.
func startShardGRPC(t *testing.T, ds *shard.DuckDBShard) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, shardservice.NewServer(ds))
	go gs.Serve(lis)
	t.Cleanup(func() { gs.GracefulStop() })
	return lis.Addr().String()
}

// startColdShardHTTP starts an httptest server that mimics the Lambda
// cold-shard query handler: accepts {sql, parquet_path}, runs the query
// against the Parquet file via DuckDB+S3, and returns protobuf-streamed
// rows. Stopped when the test ends.
func startColdShardHTTP(t *testing.T, s3cfg *shard.S3Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SQL         string `json:"sql"`
			ParquetPath string `json:"parquet_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		db, err := sql.Open("duckdb", "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer db.Close()
		if s3cfg != nil {
			if err := s3cfg.ConfigureDuckDB(db); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		query := strings.Replace(req.SQL, "FROM logs", fmt.Sprintf("FROM read_parquet('%s')", req.ParquetPath), 1)
		rows, err := db.Query(query)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		results, err := shard.ScanResultRows(rows)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		for _, row := range results {
			shard.WriteResultRow(w, row)
		}
		shard.WriteStreamEnd(w)
	}))
	t.Cleanup(func() { srv.Close() })
	return srv
}

func drainQuery(t *testing.T, c *coordinator.Coordinator, pipeline string, tr coordinator.TimeRange) []*pb.ResultRow {
	t.Helper()
	res, err := c.Execute(pipeline, tr, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute %q: %v", pipeline, err)
	}
	drained, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain %q: %v", pipeline, err)
	}
	return drained.Rows
}
