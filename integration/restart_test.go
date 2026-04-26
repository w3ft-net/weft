//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestDockerStack_CoordinatorRestart_PreservesData captures the
// production-alignment gap I hit running the demo: ingest some rows,
// restart only the coordinator container, and the rows should still be
// queryable. Today they aren't — LocalLauncher (cmd/coordinator's dev
// path) spawns ephemeral in-process shards via shardsvc.NewServer(nil),
// so when the coordinator process dies the in-memory DuckDBs go with
// it. On restart the coordinator launches *fresh* shards and the
// previous data is unreachable. This test pins the expectation that
// the coordinator's "dev" launcher needs to look more like production:
// shards as separate processes (or at least separate-lifetime) with
// persistent dataDirs the new coordinator can reattach to via the
// adopt-from-heartbeats flow.
func TestDockerStack_CoordinatorRestart_PreservesData(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping coordinator-restart test in short mode")
	}

	conn, client := grpcClient(t)
	defer conn.Close()

	token := ensureToken(t, client, "testcust", "restart-data")
	stream := openAuthedStream(t, context.Background(), client, token)

	// Tag each row with a per-invocation marker so the count
	// assertion isolates this run from any rows persisted by previous
	// runs. With DockerLauncher the shard's DuckDB is on a host
	// bind-mount that survives `docker compose down`, so accumulating
	// data is the rule rather than the exception — a strong-isolated
	// assertion is the only durable shape.
	tag := fmt.Sprintf("preserves-%d", time.Now().UnixNano())

	// Ingest a known set of rows.
	now := time.Now()
	const ingested = 10
	for i := 0; i < ingested; i++ {
		stream.Send(&pb.LogRow{
			App:      "restart-data/api",
			Customer: "testcust",
			Raw:      fmt.Sprintf("pre-restart row %d %s", i, tag),
			Level:    "INFO",
			Instance: "restart-test-host",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("close ingest: %v", err)
	}
	if resp.RowsReceived != ingested {
		t.Fatalf("ingest rowsReceived=%d, want %d", resp.RowsReceived, ingested)
	}

	// Confirm the rows are visible *before* the restart so the
	// post-restart loss is unambiguous.
	tagQuery := fmt.Sprintf("cat /log/restart-data.log | grep %s", tag)
	pre := waitForRows(t, client, tagQuery, "testcust", ingested, 30*time.Second)
	if len(pre) != ingested {
		t.Fatalf("pre-restart query: got %d rows, want %d", len(pre), ingested)
	}
	t.Logf("pre-restart: %d rows visible", len(pre))

	// Restart only the coordinator container — leaves postgres,
	// compute, and any peer shards alone. Production parity: a
	// rolling coordinator restart is supposed to be transparent to
	// active data.
	if err := dockerComposeRestart(t, "coordinator"); err != nil {
		t.Fatalf("restart coordinator: %v", err)
	}
	waitForCoordinatorReady(t, 60*time.Second)

	// Reopen the gRPC client — the old connection's address may
	// still be valid but the channel state machine doesn't always
	// pick up the bounce cleanly inside the test window.
	conn2, client2 := grpcClient(t)
	defer conn2.Close()

	post := waitForRows(t, client2, tagQuery, "testcust", ingested, 30*time.Second)
	if len(post) != ingested {
		t.Fatalf("post-restart query: got %d rows, want %d.\n"+
			"This is the production-alignment failure: LocalLauncher's "+
			"in-process shards die with the coordinator process, so the "+
			"new coordinator launches fresh empty shards and the prior "+
			"data is orphaned. Bringing dev-mode shards into a "+
			"separate-lifetime process model (subprocess cmd/shard with "+
			"persistent dataDir, or just running the static shard-0/"+
			"shard-1 compose containers and adopting their heartbeats) "+
			"is what closes the gap.", len(post), ingested)
	}
}

// TestDockerStack_CoordinatorRestart_IngestResumes simulates the
// weftd-survives-restart story at the coordinator-API level: open a
// stream, send rows, bounce the coordinator, open a fresh stream, send
// more rows, and verify the post-restart rows are queryable. This is
// the "did the coordinator surface come back ready to accept new
// ingest after a restart?" half of the question — weftd's own
// reconnect loop is unit-tested in cmd/weftd/reconnect_test.go.
//
// Today this test passes the post-restart ingest hurdle but is
// expected to FAIL on the row-count assertion at the end, because the
// pre-restart rows are stranded in the orphaned in-process shards
// (same root cause as TestDockerStack_CoordinatorRestart_PreservesData).
// Both tests will go green together once dev-mode shards persist
// across coordinator process boundaries.
func TestDockerStack_CoordinatorRestart_IngestResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping coordinator-restart-ingest test in short mode")
	}

	conn, client := grpcClient(t)
	defer conn.Close()

	// Use a namespace distinct from PreservesData's so the two tests
	// don't share data. ensureToken creates the token under this
	// namespace; rows under this namespace should be unique to this
	// test.
	token := ensureToken(t, client, "testcust", "restart-resume")
	now := time.Now()
	tag := fmt.Sprintf("resume-%d", time.Now().UnixNano())
	tagQuery := fmt.Sprintf("cat /log/restart-resume.log | grep %s", tag)

	// Phase 1: pre-restart ingest + sanity query.
	stream1 := openAuthedStream(t, context.Background(), client, token)
	const preCount = 5
	for i := 0; i < preCount; i++ {
		stream1.Send(&pb.LogRow{
			App:      "restart-resume/svc",
			Customer: "testcust",
			Raw:      fmt.Sprintf("pre-restart row %d %s", i, tag),
			Level:    "INFO",
			Instance: "restart-test-host",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}
	if _, err := stream1.CloseAndRecv(); err != nil {
		t.Fatalf("close pre stream: %v", err)
	}
	pre := waitForRows(t, client, tagQuery, "testcust", preCount, 30*time.Second)
	if len(pre) < preCount {
		t.Fatalf("pre-restart query: got %d rows, want at least %d", len(pre), preCount)
	}
	preTagged := len(pre)
	t.Logf("pre-restart: %d rows visible (raw count, may include cross-shard duplicates)", preTagged)

	// Bounce.
	if err := dockerComposeRestart(t, "coordinator"); err != nil {
		t.Fatalf("restart coordinator: %v", err)
	}
	waitForCoordinatorReady(t, 60*time.Second)

	// Phase 2: post-restart ingest. Reopen everything as if we were a
	// reconnecting weftd. The coordinator should accept the new
	// stream and write the rows somewhere queryable.
	conn2, client2 := grpcClient(t)
	defer conn2.Close()

	// Re-resolve the token in case the registry was rebuilt; the raw
	// token from before is still valid (it's hashed in the DB), but
	// the in-process map of tokens-to-streams was lost.
	stream2 := openAuthedStream(t, context.Background(), client2, token)
	const postCount = 5
	for i := 0; i < postCount; i++ {
		stream2.Send(&pb.LogRow{
			App:      "restart-resume/svc",
			Customer: "testcust",
			Raw:      fmt.Sprintf("post-restart row %d %s", i, tag),
			Level:    "INFO",
			Instance: "restart-test-host",
			TsUnixNs: now.Add(time.Duration(100+i) * time.Second).UnixNano(),
		})
	}
	resp, err := stream2.CloseAndRecv()
	if err != nil {
		t.Fatalf("close post stream: %v\n"+
			"If this is the first failure, the coordinator's restart "+
			"path isn't accepting fresh streams cleanly — check "+
			"namespace bindings and the SeenApps registry rebuild.", err)
	}
	if resp.RowsReceived != postCount {
		t.Fatalf("post-restart ingest rowsReceived=%d, want %d", resp.RowsReceived, postCount)
	}

	// After post-restart ingest the queryable count must strictly grow
	// vs the pre-restart count (every additional unique row should be
	// visible). Exact totals are brittle in this stack — multiple
	// INGESTING shards per bucket round-robin or replicate, and a tag
	// matches whichever shard the rows landed on. The contract worth
	// pinning is: pre-restart data did NOT vanish, and post-restart
	// data lands.
	got := waitForRows(t, client2, tagQuery, "testcust", preTagged+postCount, 30*time.Second)
	if len(got) < preTagged {
		t.Fatalf("combined query: got %d rows, want at least %d (pre-restart was %d).\n"+
			"Pre-restart rows are not visible after the restart — same "+
			"root cause as TestDockerStack_CoordinatorRestart_PreservesData: "+
			"shards must survive the coordinator process boundary.",
			len(got), preTagged, preTagged)
	}
	if len(got) <= preTagged {
		t.Fatalf("combined query: got %d rows, expected strictly more than %d "+
			"(post-restart ingest didn't land queryable rows)",
			len(got), preTagged)
	}
}

// dockerComposeRestart restarts a single named service from the running
// compose project. Caller waits for readiness separately.
func dockerComposeRestart(t *testing.T, service string) error {
	t.Helper()
	cmd := exec.Command("docker", "compose", "restart", service)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose restart %s: %v\noutput:\n%s",
			service, err, out)
	}
	t.Logf("docker compose restart %s: ok", service)
	return nil
}

// waitForCoordinatorReady polls the coordinator health endpoint at
// :9021 (HTTP) and the gRPC listener at :9020 until both are
// accepting traffic. The HTTP-only check isn't enough — startup races
// can leave gRPC unbound for a few hundred ms after /health goes 200,
// and a fresh stream opened in that window hits "connection reset".
func waitForCoordinatorReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := httpGetWithTimeout(healthAddr, 2*time.Second)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			if grpcAccepting(coordinatorAddr) {
				// Validate with a real authenticated unary RPC —
				// /health 200 + TCP open isn't sufficient because
				// gRPC stream handlers can race with the listener.
				if grpcUnaryWorks(t) {
					return
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("coordinator did not become healthy within %s", timeout)
}

// grpcUnaryWorks returns true if a quick authed unary call to the
// coordinator succeeds — exercises the same auth+gRPC path that
// IngestStream and Query will use, so a green here means the next
// real RPC won't hit a server-preface race.
func grpcUnaryWorks(t *testing.T) bool {
	t.Helper()
	conn, client := grpcClient(t)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.ListApps(ctx, &pb.ListAppsRequest{})
	return err == nil
}

// grpcAccepting attempts a short-lived TCP connection to the gRPC
// port. Cheap, and combined with the post-success grace sleep in the
// caller, robust against gRPC's "listener bound but server preface
// not ready" race that bites the very first stream.
func grpcAccepting(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 1*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// waitForRows runs a query repeatedly until the row count meets `want`
// or the timeout elapses, returning whatever the last response was.
// Transient gRPC errors (connection resets after a coordinator bounce,
// "unknown source" while the app registry is rebuilding from
// heartbeats, etc.) are swallowed so the caller gets the eventual
// steady-state count rather than the first transient.
func waitForRows(t *testing.T, client pb.WeftCoordinatorClient, pipeline, customer string, want int, timeout time.Duration) []*pb.ResultRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var rows []*pb.ResultRow
	for time.Now().Before(deadline) {
		rows = queryPipelineSoft(client, pipeline, customer)
		if len(rows) >= want {
			return rows
		}
		time.Sleep(2 * time.Second)
	}
	return rows
}

// queryPipelineSoft is queryPipeline without the t.Fatal — returns
// nil on any error so a poll loop can retry.
func queryPipelineSoft(client pb.WeftCoordinatorClient, pipeline, customer string) []*pb.ResultRow {
	stream, err := client.Query(context.Background(), &pb.CoordinatorQueryRequest{
		Pipeline: pipeline,
		Customer: customer,
	})
	if err != nil {
		return nil
	}
	var rows []*pb.ResultRow
	for {
		resp, err := stream.Recv()
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil
		}
		rows = append(rows, resp.Rows...)
	}
	return rows
}

// httpGetWithTimeout is a thin wrapper around http.Client.Get with a
// per-call timeout — for poll loops that shouldn't block on a hung
// connection during a coordinator restart.
func httpGetWithTimeout(url string, timeout time.Duration) (*http.Response, error) {
	c := &http.Client{Timeout: timeout}
	return c.Get(url)
}

