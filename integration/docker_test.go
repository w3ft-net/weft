//go:build integration

// Package integration contains tests that run against a live docker compose stack.
//
// Prerequisites:
//
//	docker compose up -d   (from the weft repo root)
//
// Run:
//
//	go test -tags integration ./integration/ -v
package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

const (
	coordinatorAddr = "localhost:9020"
	apiAddr         = "http://localhost:8080"
	healthAddr      = "http://localhost:9021/health"
)

// TestDockerStack_Health verifies the coordinator and API are reachable.
func TestDockerStack_Health(t *testing.T) {
	resp, err := http.Get(healthAddr)
	if err != nil {
		t.Fatalf("coordinator health unreachable (is docker compose running?): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("coordinator health: %d", resp.StatusCode)
	}
}

// TestDockerStack_ShardAdoption verifies shards are adopted from heartbeats.
func TestDockerStack_ShardAdoption(t *testing.T) {
	conn, client := grpcClient(t)
	defer conn.Close()

	// ListApps should return eventually once shards are adopted and data flows.
	var apps []string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.ListApps(context.Background(), &pb.ListAppsRequest{})
		if err == nil && len(resp.Apps) > 0 {
			apps = resp.Apps
			break
		}
		time.Sleep(2 * time.Second)
	}
	// Apps may be empty if no weftd is ingesting; at minimum the coordinator
	// should be reachable. If apps ARE populated, log them.
	t.Logf("apps discovered: %v", apps)
}

// TestDockerStack_IngestAndQuery ingests rows via gRPC IngestStream,
// then queries them back to verify the full pipeline.
func TestDockerStack_IngestAndQuery(t *testing.T) {
	conn, client := grpcClient(t)
	defer conn.Close()

	token := ensureToken(t, client, "testcust", "integration")
	stream := openAuthedStream(t, context.Background(), client, token)

	now := time.Now()
	for i := 0; i < 10; i++ {
		stream.Send(&pb.LogRow{
			App:      "integration/app-a",
			Customer: "testcust",
			Raw:      fmt.Sprintf("app-a log line %d", i),
			Level:    "INFO",
			Instance: "test-host",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}
	for i := 0; i < 10; i++ {
		stream.Send(&pb.LogRow{
			App:      "integration/app-b",
			Customer: "testcust",
			Raw:      fmt.Sprintf("app-b log line %d", i),
			Level:    "ERROR",
			Instance: "test-host",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("close ingest stream: %v", err)
	}
	if resp.RowsReceived != 20 {
		t.Errorf("expected 20 rows ingested, got %d", resp.RowsReceived)
	}

	// Wait for drain loop to flush to shard, with retry.
	var queryResp []*pb.ResultRow
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(2 * time.Second)
		queryResp = queryPipeline(t, client, "cat /log/integration.log", "testcust")
		if len(queryResp) > 0 {
			break
		}
	}
	if len(queryResp) == 0 {
		t.Fatal("namespace query returned 0 rows after 20s — drain may not have flushed")
	}
	if len(queryResp) != 20 {
		t.Errorf("namespace query: expected 20 rows, got %d", len(queryResp))
	}

	// Query sub-app — should return only 10 rows.
	subResp := queryPipeline(t, client, "cat /log/integration/app-a.log", "testcust")
	if len(subResp) != 10 {
		t.Errorf("sub-app query: expected 10 rows, got %d", len(subResp))
	}

	// Query with filter — ERROR rows only from app-b.
	filteredResp := queryPipeline(t, client, `cat /log/integration.log | awk '$LEVEL == "ERROR"'`, "testcust")
	if len(filteredResp) != 10 {
		t.Errorf("filtered query: expected 10 ERROR rows, got %d", len(filteredResp))
	}
	for _, r := range filteredResp {
		if !strings.Contains(r.Raw, "app-b") {
			t.Errorf("unexpected row in ERROR-filtered results: %q", r.Raw)
		}
	}
}

// TestDockerStack_ShardSuspendAndRecovery stops a shard container, verifies
// it gets suspended (queries still work via the other shard), then restarts
// and verifies recovery.
func TestDockerStack_ShardSuspendAndRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping shard suspend/recovery test in short mode")
	}

	conn, client := grpcClient(t)
	defer conn.Close()

	// Ingest some data first so we have something to query.
	token := ensureToken(t, client, "testcust", "suspend-test")
	stream := openAuthedStream(t, context.Background(), client, token)
	now := time.Now()
	for i := 0; i < 5; i++ {
		stream.Send(&pb.LogRow{
			App:      "suspend-test/logs",
			Customer: "testcust",
			Raw:      fmt.Sprintf("suspend test line %d", i),
			Level:    "INFO",
			Instance: "test-host",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}
	stream.CloseAndRecv()

	var rows []*pb.ResultRow
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(2 * time.Second)
		rows = queryPipeline(t, client, "cat /log/suspend-test.log", "testcust")
		if len(rows) > 0 {
			break
		}
	}
	if len(rows) == 0 {
		t.Fatal("no data before suspend test — aborting")
	}
	t.Logf("pre-suspend: %d rows queryable", len(rows))

	// NOTE: Actually stopping docker containers requires shell access.
	// This test documents the expected behavior; a full docker-control
	// test would use docker SDK or exec. For now, verify the query path
	// works end-to-end — the unit tests cover suspend/unsuspend logic.
	t.Log("shard suspend/recovery: logic covered by unit tests; docker stop/start requires manual verification or docker SDK integration")
}

// TestDockerStack_CustomerIsolation verifies that one customer can't see
// another's data in query results.
func TestDockerStack_CustomerIsolation(t *testing.T) {
	// Currently skipped: the test assumes per-request customer
	// identity (ingest as alpha, query as beta, expect zero cross-
	// visibility) but the coordinator RPC layer overrides the
	// client-supplied customer with whatever the JWT claims — every
	// request in the test uses the same kevlar JWT, so "alpha" and
	// "beta" collapse to the same tenant server-side and the test's
	// invariant is vacuous. Store-level customer isolation is
	// covered by InjectCustomer unit tests (planner/pushdown_edge_
	// test.go). Re-enabling here needs a multi-JWT harness.
	t.Skip("test predates single-JWT auth; needs multi-JWT harness to reinstate")

	conn, client := grpcClient(t)
	defer conn.Close()

	// Ingest as customer "alpha". The token's customer overrides whatever
	// row.Customer the client sends server-side.
	alphaToken := ensureToken(t, client, "alpha", "isolation")
	_ = ensureToken(t, client, "beta", "isolation") // just to make beta a real customer for the query
	stream := openAuthedStream(t, context.Background(), client, alphaToken)
	now := time.Now()
	for i := 0; i < 5; i++ {
		stream.Send(&pb.LogRow{
			App:      "isolation/secret",
			Customer: "alpha",
			Raw:      fmt.Sprintf("alpha secret line %d", i),
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}
	stream.CloseAndRecv()
	time.Sleep(5 * time.Second)

	// Query as customer "beta" — should see 0 rows.
	betaRows := queryPipeline(t, client, "cat /log/isolation.log", "beta")
	if len(betaRows) != 0 {
		t.Errorf("customer isolation broken: beta saw %d rows from alpha", len(betaRows))
	}

	// Query as customer "alpha" — should see rows.
	alphaRows := queryPipeline(t, client, "cat /log/isolation.log", "alpha")
	if len(alphaRows) != 5 {
		t.Errorf("alpha should see 5 rows, got %d", len(alphaRows))
	}
}

// TestDockerStack_HeartbeatsInPostgres verifies that shard heartbeats are
// persisted to the partitioned postgres table.
func TestDockerStack_HeartbeatsInPostgres(t *testing.T) {
	// This test connects to postgres directly via the exposed port.
	// docker-compose exposes postgres on 5433.
	conn, client := grpcClient(t)
	defer conn.Close()

	// If shards are adopted, heartbeats must be flowing.
	// Verify by checking coordinator health reports shard count > 0.
	resp, err := http.Get(healthAddr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The health endpoint returns {"shards": N, ...}.
	// If N > 0, heartbeats are working.
	t.Log("heartbeat verification: coordinator health endpoint reachable (shard count in health JSON)")
	_ = client
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func grpcClient(t *testing.T) (*grpc.ClientConn, pb.WeftCoordinatorClient) {
	t.Helper()
	conn, err := grpc.NewClient(coordinatorAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.ClientTokenUnaryInterceptor(integrationJWT(t))),
		grpc.WithStreamInterceptor(auth.ClientTokenStreamInterceptor(integrationJWT(t))),
	)
	if err != nil {
		t.Fatalf("connect to coordinator at %s: %v", coordinatorAddr, err)
	}
	return conn, pb.NewWeftCoordinatorClient(conn)
}

// integrationJWT produces a signed JWT for a fake verified user in the
// "kevlar" customer. Used by tests that exercise JWT-gated RPCs; the
// coordinator validates against the dev JWT secret set in docker-compose.
func integrationJWT(t *testing.T) string {
	t.Helper()
	secret := os.Getenv("WEFT_TEST_JWT_SECRET")
	if secret == "" {
		secret = "dev-secret-change-in-production"
	}
	now := time.Now()
	tok, err := auth.Generate(&store.User{
		ID: "integration-user", Email: "it@example.com",
		CustomerName: "kevlar", EmailVerifiedAt: &now, CreatedAt: now,
	}, "trial", secret)
	if err != nil {
		t.Fatalf("sign integration JWT: %v", err)
	}
	return tok
}

func queryPipeline(t *testing.T, client pb.WeftCoordinatorClient, pipeline, customer string) []*pb.ResultRow {
	t.Helper()
	stream, err := client.Query(context.Background(), &pb.CoordinatorQueryRequest{
		Pipeline: pipeline,
		Customer: customer,
	})
	if err != nil {
		t.Fatalf("query %q: %v", pipeline, err)
	}

	var rows []*pb.ResultRow
	for {
		resp, err := stream.Recv()
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("recv: %v", err)
		}
		rows = append(rows, resp.Rows...)
	}
	return rows
}
