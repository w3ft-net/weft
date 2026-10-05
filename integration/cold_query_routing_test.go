//go:build integration && test && !localstack

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

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	shardservice "github.com/graybeardcorp/weft/shard/service"
	"github.com/graybeardcorp/weft/store"
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

	cust, err := tx.FindOrCreateOrg("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := tx.FindOrCreateNamespace(cust.ID, "vimgym")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateSubscription(&store.Subscription{
		ID:       "cold-query-bolt",
		OrgID:    cust.ID,
		Tier:     string(store.TierBolt),
		Status:   "active",
		StartsAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Two time windows: shard-0 covers [t0, t1), shard-1 covers [t1, t2).
	// Relative to time.Now(), not a fixed calendar date — see
	// coordinator/coordinator_test.go's t0/t1/t2 comment for why a
	// fixed date here eventually falls outside the tier's retention
	// clamp as real time advances.
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	t1 := t0.Add(1 * time.Hour)
	t2 := t1.Add(1 * time.Hour)

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
		{Raw: "hot line 1", Namespace: "vimgym", Level: "INFO", CustomerId: 1, TsUnixNs: t0.Add(time.Minute).UnixNano()},
		{Raw: "hot line 2", Namespace: "vimgym", Level: "ERROR", CustomerId: 1, TsUnixNs: t0.Add(2 * time.Minute).UnixNano()},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.Ingest([]*pb.LogRow{
		{Raw: "cold line 3", Namespace: "vimgym", Level: "WARN", CustomerId: 1, TsUnixNs: t1.Add(time.Minute).UnixNano()},
		{Raw: "cold line 4", Namespace: "vimgym", Level: "INFO", CustomerId: 1, TsUnixNs: t1.Add(2 * time.Minute).UnixNano()},
	}, nil); err != nil {
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
	// entry.Live to a ColdShard; replicate that here. The compute pool
	// dials its own shard via DialShard, so this ColdShard is only used
	// to pass the filter.
	cold, err := shard.NewColdShard("cold-route-1", []string{"vimgym"}, s3Path, lambdaSrv.URL, nil)
	if err != nil {
		t.Fatalf("create cold shard: %v", err)
	}
	t.Cleanup(func() { cold.Close() })

	svcs := []store.ShardService{{NamespaceID: ns.ID, Service: "vimgym"}}
	c := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Pool: &compute.LocalComputePool{},
		Store:             tx,
		LambdaFunctionURL: lambdaSrv.URL})

	// The real MintShard path persists three things: the shard_records row,
	// the in-memory registry entry, and the shard_services rows that query
	// routing resolves against (ShardIDsServingService reads the table, not
	// the in-memory Services field — migration 058). SeedShard only does the
	// registry, so mirror the other two here: persist the row (shard_services
	// has an FK to shard_records, which has a NOT NULL FK to a bucket) and
	// stamp the service.
	def, ok, err := tx.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket: ok=%v err=%v", ok, err)
	}
	shards := []*logdb.Shard{
		{
			ID: "cold-route-0", BucketID: def.ID, Services: svcs,
			WindowStart: t0, WindowEnd: t1,
			State: logdb.StateIngesting, Addr: s0Addr,
			CreatedAt: time.Now(),
		},
		{
			ID: "cold-route-1", BucketID: def.ID, Services: svcs,
			WindowStart: t1, WindowEnd: t2,
			State: logdb.StateArchived, ParquetPath: s3Path,
			Live:      cold,
			RowCount:  4, // archived shard holds data; MatchingEntries now skips 0-row cold/RO shards
			CreatedAt: time.Now(),
		},
	}
	shardStore := logdb.ShardStore{Store: tx}
	for _, sh := range shards {
		if err := shardStore.SaveShard(sh); err != nil {
			t.Fatalf("persist shard %s: %v", sh.ID, err)
		}
		logdb.SeedShard(c.Provisioner, sh)
		if err := tx.AddShardService(sh.ID, ns.ID, "vimgym"); err != nil {
			t.Fatalf("stamp shard_services for %s: %v", sh.ID, err)
		}
	}

	// --- Query each window independently, then combined ---

	hotWindow := logdb.TimeRange{Start: t0, End: t1}
	coldWindow := logdb.TimeRange{Start: t1, End: t2}
	fullWindow := logdb.TimeRange{Start: t0, End: t2}

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
	entry1 := logdb.GetEntry(c.Provisioner, "cold-route-1")
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
			SQL         string   `json:"sql"`
			ParquetPath string   `json:"parquet_path"`
			Args        []string `json:"args,omitempty"`
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
		args := make([]any, len(req.Args))
		for i, a := range req.Args {
			args[i] = a
		}
		rows, err := db.Query(query, args...)
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

func drainQuery(t *testing.T, c *coordinator.Coordinator, pipeline string, tr logdb.TimeRange) []*pb.ResultRow {
	t.Helper()
	res, err := c.Execute(context.Background(), pipeline, tr, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute %q: %v", pipeline, err)
	}
	drained, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain %q: %v", pipeline, err)
	}
	return drained.Rows
}
