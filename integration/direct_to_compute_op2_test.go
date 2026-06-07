//go:build integration && cover && !localstack

// Direct-to-compute op 2: compute's k-way merge across shard input
// streams when CreateJobRequest.MergeKey is set. See
// docs/plans/direct-to-compute.md §4. Gated behind the `cover` tag
// because the test dials shard-0/shard-1 on localhost — those host
// port mappings come with the docker-compose.cover.yml overlay. Run
// via scripts/cover-test.sh.
//
// Op 1 landed the shard side: one shard streams its rows (already in
// local ORDER BY) to compute. Op 2 glues multiple shard streams
// together inside compute. With two shards each emitting its own
// sorted ts range, the merged output must interleave by ts. The
// existing concat path (merge_key="") drops rows in arrival order,
// which for interleaved shard data is a jumble.
//
// This test drives two docker shards with *disjoint, interleavable*
// timestamp ranges (odd ns → shard-0, even ns → shard-1) and asserts
// the final compute output is globally sorted by ts. Against the
// pre-op-2 concat-only implementation the assertion fails because the
// rows land in shard-arrival order, not merged order.
package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

const (
	shard1HostPort = "localhost:9002"
)

func TestDirectToCompute_Op2_ComputeMergesAcrossShards(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two salted prefixes so repeat test runs don't trip over rows
	// that linger in the persistent shard volumes.
	now := time.Now().UnixNano()
	prefix := fmt.Sprintf("op2-%d", now)

	// ---- 1. Configure each shard with a distinct identity ----------
	shard0Conn := dialShard(t, shard0HostPort)
	defer shard0Conn.Close()
	shard0Client := pb.NewShardQueryClient(shard0Conn)
	configureIfNeeded(t, ctx, shard0Client, "test-op2-shard-0")

	shard1Conn := dialShard(t, shard1HostPort)
	defer shard1Conn.Close()
	shard1Client := pb.NewShardQueryClient(shard1Conn)
	configureIfNeeded(t, ctx, shard1Client, "test-op2-shard-1")

	// ---- 2. Split timestamps across the two shards -----------------
	//
	// Odd ns (1,3,5,…,19) → shard-0
	// Even ns (2,4,6,…,20) → shard-1
	// Each shard's SQL is "ORDER BY ts ASC" so its emitted stream is
	// monotonically increasing within the shard. The global merge of
	// the two streams is 1..20.
	const perShardRows = 10
	baseTs := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	ingestInterleaved(t, ctx, shard0Client, prefix, baseTs, func(i int) int { return 2*i + 1 }, perShardRows)
	ingestInterleaved(t, ctx, shard1Client, prefix, baseTs, func(i int) int { return 2*i + 2 }, perShardRows)

	// ---- 3. Create a compute job with merge_key set ----------------
	//
	// ShardCount=2 so compute waits for both sentinels. MergeKey set
	// to "ts" to request the k-way merge path.
	computeConn := dialCompute(t, computeAddrHostPort)
	defer computeConn.Close()
	computeClient := pb.NewWeftComputeClient(computeConn)

	jobResp, err := computeClient.CreateJob(ctx, &pb.CreateJobRequest{
		Stages:     []string{"cat"},
		ShardCount: 2,
		MergeKey:   "ts",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// ---- 4. Fan out QueryToCompute to both shards in parallel ------
	//
	// Both shard containers dial compute at "compute:9010" — the same
	// job_id, so compute's SendRows handler buckets by msg.ShardId.
	// Each shard's SQL includes ORDER BY ts so its own stream is
	// pre-sorted — what compute has to merge, not re-sort.
	sql := fmt.Sprintf(
		"SELECT raw, ts FROM logs WHERE raw LIKE '%s-%%' ORDER BY ts",
		prefix,
	)

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	launch := func(client pb.ShardQueryClient, label string) {
		defer wg.Done()
		resp, err := client.QueryToCompute(ctx, &pb.QueryToComputeRequest{
			Sql:         sql,
			ComputeAddr: computeAddrDocker,
			JobId:       jobResp.JobId,
		})
		if err != nil {
			errs <- fmt.Errorf("%s: %w", label, err)
			return
		}
		if resp.RowsSent != int64(perShardRows) {
			errs <- fmt.Errorf("%s: rows_sent=%d want %d", label, resp.RowsSent, perShardRows)
			return
		}
	}
	go launch(shard0Client, "shard-0")
	go launch(shard1Client, "shard-1")
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	// ---- 5. GetResult and verify the merge ordering ----------------
	result, err := computeClient.GetResult(ctx, &pb.GetResultRequest{JobId: jobResp.JobId})
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("compute error: %s", result.Error)
	}

	// The "cat" stage is an identity filter, so result.Text is one
	// row per line, each row.Raw was "<prefix>-<seqN>".
	gotSeqs := parseSeqNums(t, result.Text, prefix)

	if len(gotSeqs) != 2*perShardRows {
		t.Fatalf("expected %d total rows, got %d. Text:\n%s", 2*perShardRows, len(gotSeqs), result.Text)
	}

	// The merge assertion: rows must come out in ascending sequence
	// order (which mirrors ts order since seq N maps to baseTs+N*ms).
	// Concat-only (pre-op-2) fails here because each shard's block
	// lands contiguously in arrival order, not interleaved.
	for i := 1; i < len(gotSeqs); i++ {
		if gotSeqs[i] <= gotSeqs[i-1] {
			t.Fatalf(
				"merge not in sorted order: gotSeqs[%d]=%d, gotSeqs[%d]=%d.\n"+
					"Full sequence: %v\n"+
					"(With merge_key=\"ts\", compute should k-way merge the two shard streams.)",
				i-1, gotSeqs[i-1], i, gotSeqs[i], gotSeqs,
			)
		}
	}
}

// TestDirectToCompute_Op2_BoundaryCoverage is the compute-side
// counterpart of op 1's shard-side assertion: the k-way merge must
// execute in the compute container. If this ever lights up with the
// wrong service's profile, someone moved merge logic back into the
// coordinator binary — which defeats the whole plan.
//
// Asserts mergeRowsFull (the row-preserving merge) since that's what
// LocalComputeJob.consume calls directly after Phase 2; the older
// mergeRows is now a thin wrapper used only by unit tests inside the
// compute package, never by the compute service binary.
func TestDirectToCompute_Op2_BoundaryCoverage(t *testing.T) {
	RequireRanIn(t, "compute", "compute/merge.go", "mergeRowsFull")
}

// configureIfNeeded christens a shard that doesn't already carry an
// identity. Matches the behavior in the op 1 test: docker-compose's
// local-launcher provisioner doesn't touch the gRPC shard containers,
// so the tests drive Configure themselves. Idempotent via the
// "already configured" error string the shard returns.
func configureIfNeeded(t *testing.T, ctx context.Context, client pb.ShardQueryClient, shardID string) {
	t.Helper()
	_, err := client.Configure(ctx, &pb.ConfigureRequest{
		ShardId:        shardID,
		Services:       []string{"directcompute/op2"},
		WindowStart:    "2026-04-23T00:00:00Z",
		WindowEnd:      "2026-04-24T00:00:00Z",
		HeartbeatToken: "dev-test-token",
	})
	if err != nil && !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("Configure %s: %v", shardID, err)
	}
}

// ingestInterleaved pushes n rows to a shard with timestamps derived
// from seqFn(i). Raw is "<prefix>-<seq>" so the result parser can
// recover the sequence number from the compute output even when the
// "cat" stage strips structure. Timestamps are baseTs + seq*ms so the
// sequence order equals ts order.
func ingestInterleaved(
	t *testing.T,
	ctx context.Context,
	client pb.ShardQueryClient,
	prefix string,
	baseTs time.Time,
	seqFn func(i int) int,
	n int,
) {
	t.Helper()
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		seq := seqFn(i)
		rows[i] = &pb.LogRow{
			Raw:       fmt.Sprintf("%s-%d", prefix, seq),
			Namespace: "directcompute", Service: "op2",
			Level:    "INFO",
			Customer: "kevlar",
			TsUnixNs: baseTs.Add(time.Duration(seq) * time.Millisecond).UnixNano(),
		}
	}
	resp, err := client.Ingest(ctx, &pb.IngestRequest{Rows: rows})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if resp.RowsIngested != int64(n) {
		t.Fatalf("ingest rows_ingested=%d want %d", resp.RowsIngested, n)
	}
}

// parseSeqNums splits compute's text output on newlines and extracts
// the numeric suffix of each line. Our test raws are "<prefix>-<N>",
// so splitting after the last "-" gets us the sequence number back.
func parseSeqNums(t *testing.T, text, prefix string) []int {
	t.Helper()
	out := []int{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, prefix+"-") {
			t.Fatalf("unexpected row in compute output: %q", line)
		}
		suf := strings.TrimPrefix(line, prefix+"-")
		n, err := strconv.Atoi(suf)
		if err != nil {
			t.Fatalf("parse seq from %q: %v", line, err)
		}
		out = append(out, n)
	}
	return out
}
