//go:build integration && !localstack

// Direct-to-compute op 5 coverage: the coordinator's executeQuery
// compute-stage branch fans out per-shard queries directly into the
// compute job instead of buffering everything in coordinator RAM.
// See docs/plans/direct-to-compute.md §§4-5.
//
// This test exercises the full stack through the coordinator's Query
// RPC: ingest rows, issue a pipeline with a compute stage, assert
// the returned text is what the pipeline would produce. It passes
// against both the legacy buffered path and the new direct path —
// the safety is that we never miss/reorder rows. Op 5's real win is
// coordinator memory (O(1) per query instead of O(rows)), which is
// hard to observe directly; the boundary-coverage check below pins
// the new code paths as actually executed.
package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

func TestDirectToCompute_Op5_CoordinatorComputeStageRoundtrip(t *testing.T) {
	conn, client := grpcClient(t)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ingest rows that straddle two shards in the local launcher's
	// shard fleet. We don't care which shard gets which row — the
	// direct-to-compute path merges whatever the coordinator's
	// MatchingShards returns.
	token := ensureToken(t, client, "kevlar", "op5compute")
	stream := openAuthedStream(t, ctx, client, token)

	now := time.Now()
	unique := fmt.Sprintf("op5-%d", now.UnixNano())
	// Ingest out-of-order Raw values. With the compute-side `sort`
	// stage the final output must be ASCII-sorted regardless of
	// arrival or shard layout.
	values := []string{
		unique + "-delta",
		unique + "-alpha",
		unique + "-charlie",
		unique + "-bravo",
		unique + "-echo",
	}
	for i, v := range values {
		stream.Send(&pb.LogRow{
			Namespace: "op5compute", Service: "app",
			Customer: "kevlar",
			Raw:      v,
			Level:    "INFO",
			Instance: "test-host",
			TsUnixNs: now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("ingest close: %v", err)
	}

	// Let drain flush rows into shards.
	var text string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		t.Log("waiting for drain...")
		time.Sleep(2 * time.Second)

		qstream, err := client.Execute(ctx, &pb.CoordinatorExecuteRequest{
			Pipeline: `cat /log/op5compute.log | grep "` + unique + `" | sort`,
			Customer: "kevlar",
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}

		var buf strings.Builder
		for {
			resp, err := qstream.Recv()
			if err != nil {
				break
			}
			// All output now flows as Lines (the wire dropped the
			// rows/text duality). Compute pipelines emit one Line per
			// rendered output row; LineNum is 0 because compute
			// strips row metadata.
			for _, line := range resp.Lines {
				buf.WriteString(line.Raw)
				buf.WriteByte('\n')
			}
		}
		if buf.Len() > 0 {
			text = buf.String()
			break
		}
	}
	if text == "" {
		t.Fatal("compute response empty — drain may not have flushed")
	}

	// Each line of text is a row's Raw value, globally sorted.
	gotLines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	// Filter to only our unique-prefixed rows (other tests may have
	// left rows in the shared shards).
	var ours []string
	for _, l := range gotLines {
		if strings.Contains(l, unique) {
			ours = append(ours, l)
		}
	}
	if len(ours) != len(values) {
		t.Fatalf("expected %d rows, got %d:\n%s", len(values), len(ours), strings.Join(ours, "\n"))
	}

	wantSorted := append([]string(nil), values...)
	sort.Strings(wantSorted)
	for i, got := range ours {
		if got != wantSorted[i] {
			t.Errorf("row %d: got %q, want %q", i, got, wantSorted[i])
		}
	}
}

// TestDirectToCompute_Op5_BoundaryCoverage moved to
// direct_to_compute_op5_cover_test.go under the `cover` build tag
// since it depends on coverage/coordinator.out being on disk.
