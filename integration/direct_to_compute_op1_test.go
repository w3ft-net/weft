//go:build integration

// Direct-to-compute op 1: shard's QueryToCompute RPC streams rows
// directly to a compute worker. See docs/plans/direct-to-compute.md.
//
// This test dials shard-0 and compute directly — the coordinator is
// deliberately *not* on the RPC path. If data ends up in the compute
// job's Result, it's because shard-0 did the work: queried DuckDB
// in-process, opened a connection to compute, streamed rows, and sent
// a sentinel. That rules out the "coordinator did the query and
// forwarded" regression that defines the entire plan.
//
// Against the placeholder stub in shard/service/server.go:221 this
// test fails — the stub returns `&QueryToComputeResponse{}` with
// rows_sent=0 and never contacts compute, so both the rows_sent
// assertion and the compute GetResult both diagnose the gap.
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

const (
	shard0HostPort = "localhost:9000"
	computeAddrHostPort = "localhost:9010"

	// Address the shard container uses to reach the compute container
	// over the docker network. Must be resolvable *inside* the shard
	// container, not from the host — the shard dials this itself.
	computeAddrDocker = "compute:9010"
)

// TestDirectToCompute_Op1_ShardStreamsDirectlyToCompute is the op 1
// regression test: shard.QueryToCompute pushes rows into a pre-created
// compute job without the coordinator's help. It proves (a) shard-0
// actually runs the query and (b) compute assembles the stream.
func TestDirectToCompute_Op1_ShardStreamsDirectlyToCompute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ---- 1. Configure shard-0 with a test identity ----------------
	//
	// docker-compose's provisioner doesn't christen the gRPC shards in
	// the local-launcher mode used by dev/docker (it runs in-process
	// DuckDB shards instead), so the shard-0 container sits blank. We
	// drive Configure ourselves. Idempotent: already-configured returns
	// an error we match and ignore.
	shardConn := dialShard(t, shard0HostPort)
	defer shardConn.Close()
	shardClient := pb.NewShardQueryClient(shardConn)

	testShardID := "test-direct-to-compute-op1"
	_, err := shardClient.Configure(ctx, &pb.ConfigureRequest{
		ShardId:        testShardID,
		Apps:           []string{"directcompute/app"},
		WindowStart:    "2026-04-23T00:00:00Z",
		WindowEnd:      "2026-04-24T00:00:00Z",
		HeartbeatToken: "dev-test-token",
	})
	if err != nil && !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("Configure shard-0: %v", err)
	}

	// ---- 2. Ingest test rows directly into shard-0 -----------------
	const nRows = 10
	rows := make([]*pb.LogRow, nRows)
	now := time.Now()
	for i := range rows {
		rows[i] = &pb.LogRow{
			LineNum:  int64(i + 1),
			Raw:      fmt.Sprintf("op1-row-%d", i),
			App:      "directcompute/app",
			Level:    "INFO",
			Customer: "kevlar",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		}
	}
	ingestResp, err := shardClient.Ingest(ctx, &pb.IngestRequest{Rows: rows})
	if err != nil {
		t.Fatalf("shard-0 ingest: %v", err)
	}
	if ingestResp.RowsIngested != nRows {
		t.Fatalf("expected %d rows ingested, got %d", nRows, ingestResp.RowsIngested)
	}

	// ---- 3. Create a compute job expecting 1 shard ----------------
	computeConn := dialCompute(t, computeAddrHostPort)
	defer computeConn.Close()
	computeClient := pb.NewWeftComputeClient(computeConn)

	jobResp, err := computeClient.CreateJob(ctx, &pb.CreateJobRequest{
		Stages:     []string{"cat"}, // identity pipeline: result == input
		ShardCount: 1,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if jobResp.JobId == "" {
		t.Fatalf("CreateJob returned empty job_id")
	}

	// ---- 4. Tell shard-0 to stream to compute ---------------------
	//
	// We pass the *docker network* address for compute, since the
	// shard will dial it from inside its container. Passing
	// "localhost:9010" would only work if compute were in the same
	// container, which it isn't.
	qtcResp, err := shardClient.QueryToCompute(ctx, &pb.QueryToComputeRequest{
		Sql:         "SELECT line_num, raw FROM logs WHERE raw LIKE 'op1-row-%' ORDER BY line_num",
		ComputeAddr: computeAddrDocker,
		JobId:       jobResp.JobId,
	})
	if err != nil {
		t.Fatalf("QueryToCompute: %v", err)
	}
	if qtcResp.RowsSent != nRows {
		t.Fatalf("QueryToCompute rows_sent = %d, want %d. Stub returns 0 — did op 1 get implemented?", qtcResp.RowsSent, nRows)
	}

	// ---- 5. Wait for compute to finish and read the result --------
	//
	// If the shard didn't send a sentinel (e.g. stub path), compute
	// blocks forever on its shard-count gate — the context deadline
	// is what turns that into a test failure rather than a hang.
	resultResp, err := computeClient.GetResult(ctx, &pb.GetResultRequest{JobId: jobResp.JobId})
	if err != nil {
		t.Fatalf("GetResult: %v (did the shard send a sentinel?)", err)
	}
	if resultResp.Error != "" {
		t.Fatalf("GetResult returned compute error: %s", resultResp.Error)
	}

	// The "cat" stage passes rows through unchanged. Each row.Raw is
	// on its own line. Just count lines and spot-check a few.
	gotLines := strings.Count(strings.TrimRight(resultResp.Text, "\n"), "\n") + 1
	if resultResp.Text == "" {
		gotLines = 0
	}
	if gotLines != nRows {
		t.Fatalf("compute result has %d lines, want %d. Text:\n%s", gotLines, nRows, resultResp.Text)
	}
	for i := 0; i < nRows; i++ {
		needle := fmt.Sprintf("op1-row-%d", i)
		if !strings.Contains(resultResp.Text, needle) {
			t.Errorf("compute result missing %q. Full text:\n%s", needle, resultResp.Text)
		}
	}
}

// TestDirectToCompute_Op1_BoundaryCoverage is a belt-and-suspenders
// cross-check that `QueryToCompute` actually ran in the shard-0
// container — not accidentally picked up by the coordinator. It skips
// when coverage/shard-0.out is missing (i.e. the test wasn't run with
// the docker-compose.cover.yml overlay + scripts/coverage-report.sh).
//
// NB: the stub's two-line body produces 100% coverage for
// `QueryToCompute` too, so this assertion alone doesn't distinguish
// stub from real impl — the functional test above does. This is a
// *boundary* check: makes sure whichever code did run, ran in the
// right process.
func TestDirectToCompute_Op1_BoundaryCoverage(t *testing.T) {
	RequireRanIn(t, "shard-0", "shard/service/server.go", "QueryToCompute")
}

func dialShard(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	// Match docker-compose.yml's SHARD_SECRET value.
	const devSecret = "dev-shard-secret"
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.ClientTokenUnaryInterceptor(devSecret)),
		grpc.WithStreamInterceptor(auth.ClientTokenStreamInterceptor(devSecret)),
	)
	if err != nil {
		t.Fatalf("dial shard %s: %v", addr, err)
	}
	return conn
}

func dialCompute(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial compute %s: %v", addr, err)
	}
	return conn
}
