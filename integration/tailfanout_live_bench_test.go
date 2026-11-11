//go:build integration && !localstack

// Package integration: throwaway pre-prod benchmark for the real
// cross-coordinator tail -f fan-out (docs/plans/coordinator-horizontal-scaling.md
// item 4). Unlike bench/tailfanout_bench_test.go — which wires two
// simulated in-process Node structs directly to a candidate transport, no
// gRPC, no real ingest path — this exercises the actual wire path that
// will run in prod: real IngestStream → coordinator's ring buffer →
// TailFanout.Publish → real Valkey PUBLISH → coordinator2's PSUBSCRIBE →
// TailBroker.Broadcast → a real Execute(follow=true) gRPC stream.
//
// Not part of the default integration run: requires a second coordinator
// (`docker compose up -d coordinator2`) and is gated behind
// WEFT_BENCH_TAILFANOUT_LIVE=1 since it's slower and noisier than a
// correctness test. Run before shipping any change that touches
// coordinator/tail_fanout*.go, to catch a real-network regression the
// in-process bench can't see (docker bridge hop, actual Valkey round
// trip, gRPC stream-framing overhead).
package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/graybeardcorp/weft/grpctoken"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

const (
	coordinator2Addr   = "localhost:9023"
	coordinator2Health = "http://localhost:9024/health"
)

func TestTailFanoutLive_CrossCoordinatorBenchmark(t *testing.T) {
	if os.Getenv("WEFT_BENCH_TAILFANOUT_LIVE") != "1" {
		t.Skip("set WEFT_BENCH_TAILFANOUT_LIVE=1 to run the live cross-coordinator tail-fanout benchmark (needs `docker compose up -d coordinator2`)")
	}
	if resp, err := http.Get(coordinator2Health); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("coordinator2 not reachable at %s (start it with `docker compose up -d coordinator2`): %v", coordinator2Health, err)
	}

	ingestConn, ingestClient := grpcClient(t)
	defer ingestConn.Close()
	followConn, followClient := grpcClientAt(t, coordinator2Addr)
	defer followConn.Close()

	t.Run("latency", func(t *testing.T) {
		const n = 100
		const spacing = 15 * time.Millisecond
		lat := runFanoutBench(t, ingestClient, followClient, n, spacing)

		if lat.delivered < n {
			t.Errorf("latency run: only %d/%d rows crossed coordinators via Redis within %s", lat.delivered, n, lat.waitBudget)
		}
		t.Logf("latency  n=%d delivered=%d/%d  p50=%s p95=%s p99=%s max=%s",
			n, lat.delivered, n, lat.p50, lat.p95, lat.p99, lat.max)
	})

	t.Run("throughput", func(t *testing.T) {
		const n = 2000
		res := runFanoutBench(t, ingestClient, followClient, n, 0)

		deliveredPct := float64(res.delivered) / float64(n) * 100
		if deliveredPct < 95 {
			t.Errorf("throughput run: only delivered %.1f%% (%d/%d) within %s — cross-coordinator fan-out is dropping rows under burst",
				deliveredPct, res.delivered, n, res.waitBudget)
		}
		t.Logf("throughput n=%d delivered=%d/%d (%.1f%%)  send_span=%s  effective=%.0f rows/s  p50=%s p95=%s p99=%s max=%s",
			n, res.delivered, n, deliveredPct, res.sendSpan, float64(res.delivered)/res.sendSpan.Seconds(),
			res.p50, res.p95, res.p99, res.max)
	})
}

type fanoutBenchResult struct {
	delivered          int
	sendSpan           time.Duration
	waitBudget         time.Duration
	p50, p95, p99, max time.Duration
}

// runFanoutBench ingests n rows against the primary coordinator, tagging
// each with its own send-side origin timestamp, and measures how long
// each takes to arrive on a live `tail -f` session against a *different*
// coordinator (coordinator2) — the only way that row can have gotten
// there is via Redis. spacing == 0 sends as fast as IngestStream.Send
// allows (a throughput burst); spacing > 0 paces sends so each row's
// latency is measured independently of queueing behind the others.
func runFanoutBench(
	t *testing.T,
	ingestClient, followClient pb.WeftCoordinatorClient,
	n int, spacing time.Duration,
) fanoutBenchResult {
	t.Helper()

	ns := "integration"
	svc := fmt.Sprintf("tailfanout-bench-%d", time.Now().UnixNano())

	followCtx, cancelFollow := context.WithCancel(context.Background())
	defer cancelFollow()
	followStream, err := followClient.Execute(followCtx, &pb.CoordinatorExecuteRequest{
		Pipeline: fmt.Sprintf("tail %s/%s/main.log", ns, svc),
		Follow:   true,
	})
	if err != nil {
		t.Fatalf("open follow stream on coordinator2: %v", err)
	}

	arrivals := make(chan time.Duration, n)
	go func() {
		for {
			resp, err := followStream.Recv()
			if err != nil {
				return
			}
			now := time.Now()
			for _, l := range resp.Lines {
				var seq int
				var originNs int64
				if _, err := fmt.Sscanf(l.Raw, "tailfanout-bench-row %d %d", &seq, &originNs); err != nil {
					continue // historical/unrelated line, not one of ours
				}
				arrivals <- now.Sub(time.Unix(0, originNs))
			}
		}
	}()

	// Give ExecuteFollow's historical phase and the resulting
	// PSUBSCRIBE(coordinator2 -> Valkey) time to land before publishing
	// — a real network round trip, unlike the in-process bench.
	time.Sleep(500 * time.Millisecond)

	token := ensureToken(t, ingestClient, "kevlar", ns)
	ingestStream := openAuthedStream(t, context.Background(), ingestClient, token)

	sendStart := time.Now()
	for i := 0; i < n; i++ {
		origin := time.Now()
		if err := ingestStream.Send(&pb.LogRow{
			Namespace: ns, Service: svc, Kind: "main",
			Raw:      fmt.Sprintf("tailfanout-bench-row %d %d", i, origin.UnixNano()),
			Instance: "tailfanout-bench",
			TsUnixNs: origin.UnixNano(),
		}); err != nil {
			t.Fatalf("send row %d: %v", i, err)
		}
		if spacing > 0 {
			time.Sleep(spacing)
		}
	}
	sendSpan := time.Since(sendStart)
	if _, err := ingestStream.CloseAndRecv(); err != nil {
		t.Fatalf("close ingest stream: %v", err)
	}

	waitBudget := 10 * time.Second
	deadline := time.After(waitBudget)
	var latencies []time.Duration
collect:
	for len(latencies) < n {
		select {
		case d := <-arrivals:
			latencies = append(latencies, d)
		case <-deadline:
			break collect
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(p * float64(len(latencies)-1))
		return latencies[idx]
	}
	var max time.Duration
	if len(latencies) > 0 {
		max = latencies[len(latencies)-1]
	}

	return fanoutBenchResult{
		delivered:  len(latencies),
		sendSpan:   sendSpan,
		waitBudget: waitBudget,
		p50:        pct(0.50),
		p95:        pct(0.95),
		p99:        pct(0.99),
		max:        max,
	}
}

// grpcClientAt is grpcClient (docker_test.go) parameterized on address, so
// this file can talk to coordinator2 as well as the primary coordinator.
func grpcClientAt(t *testing.T, addr string) (*grpc.ClientConn, pb.WeftCoordinatorClient) {
	t.Helper()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(grpctoken.ClientTokenUnaryInterceptor(integrationJWT(t))),
		grpc.WithStreamInterceptor(grpctoken.ClientTokenStreamInterceptor(integrationJWT(t))),
	)
	if err != nil {
		t.Fatalf("connect to coordinator at %s: %v", addr, err)
	}
	return conn, pb.NewWeftCoordinatorClient(conn)
}
