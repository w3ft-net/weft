//go:build integration

package bench

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/graybeardcorp/weft/bench/tailfanout"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// This file benchmarks the Redis Pub/Sub tail -f cross-coordinator
// fan-out candidate from docs/plans/coordinator-horizontal-scaling.md
// item 4 against simulated multi-coordinator clusters built from
// bench/tailfanout. It does not exercise the real coordinator over gRPC —
// these are synthetic, in-process nodes, each wrapping a real, unmodified
// coordinator.TailBroker. (A gRPC-mesh candidate was also benchmarked
// here and lost on latency and backpressure-vs-N scaling; its code was
// deleted once Redis was picked — see the results table in the design
// doc.)
//
// Run with: WEFT_BENCH_TAILFANOUT=1 go test -tags integration ./bench/...
// -run TestTailFanoutBenchmarkSuite -v
//
// Gated behind an extra env var (on top of -tags integration) because the
// throughput-ceiling ramp, run at N=2/3/5, is much slower than the rest
// of this package's benchmarks. Kept as an ongoing perf-regression
// harness now that Redis is the production transport.

const fanoutTopicNamespace = "bench-ns"
const fanoutTopicService = "bench-svc"
const fanoutTopicKind = "main"

func fanoutMatcher() *coordinator.RowMatcher {
	return &coordinator.RowMatcher{
		Namespace: fanoutTopicNamespace,
		Service:   fanoutTopicService,
		Kind:      fanoutTopicKind,
	}
}

func syntheticRow() *pb.LogRow {
	return &pb.LogRow{
		Namespace: fanoutTopicNamespace,
		Service:   fanoutTopicService,
		Kind:      fanoutTopicKind,
		Raw:       "synthetic tail-fanout benchmark row",
		TsUnixNs:  time.Now().UnixNano(),
	}
}

// requireRedis is a soft, per-(sub)test skip — unlike requireCoordinator's
// hard TestMain-level gate, this only skips the fanout suite itself, not
// the rest of the package's benchmarks.
func requireRedis(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("WEFT_BENCH_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6380"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable at %s (start it with `docker compose up -d redis`): %v", addr, err)
	}
	return addr
}

func newRedisCluster(t *testing.T, n int, addr string) []*tailfanout.Node {
	t.Helper()
	nodes := make([]*tailfanout.Node, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("redis-%d", i)
		rt, err := tailfanout.NewRedisTransport(id, addr)
		if err != nil {
			t.Fatalf("new redis transport %s: %v", id, err)
		}
		nodes[i] = tailfanout.NewNode(id, rt)
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.Close()
		}
	})
	return nodes
}

// percentile expects sorted ascending.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func median(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// measureLatency publishes count origin-stamped rows one at a time from
// nodes[0] to a single subscriber on nodes[1] (a different node — isolates
// cross-node hop cost from same-process TailBroker.Broadcast cost) and
// reports p50/p95/p99 arrival latency.
func measureLatency(t *testing.T, nodes []*tailfanout.Node, count int) (p50, p95, p99 time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := nodes[1]
	id, ch := sub.Subscribe(ctx, fanoutMatcher())
	defer sub.Unsubscribe(id, fanoutMatcher())

	deltas := make([]time.Duration, 0, count)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < count; i++ {
			select {
			case row := <-ch:
				deltas = append(deltas, time.Since(time.Unix(0, row.TsUnixNs)))
			case <-time.After(10 * time.Second):
				return
			}
		}
	}()

	pub := nodes[0]
	for i := 0; i < count; i++ {
		pub.Publish([]*pb.LogRow{syntheticRow()})
	}
	<-done

	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	return percentile(deltas, 0.50), percentile(deltas, 0.95), percentile(deltas, 0.99)
}

// measureThroughputCeiling ramps the publish rate from 1k rows/sec upward
// (no grounded production target exists yet, per the plan doc's decision)
// until the drop rate at a subscriber on a different node crosses 1%, and
// reports the last sustained rate.
func measureThroughputCeiling(t *testing.T, nodes []*tailfanout.Node) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := nodes[1]
	id, ch := sub.Subscribe(ctx, fanoutMatcher())
	defer sub.Unsubscribe(id, fanoutMatcher())

	var received atomic.Int64
	stopDrain := make(chan struct{})
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		for {
			select {
			case <-ch:
				received.Add(1)
			case <-stopDrain:
				return
			}
		}
	}()
	defer func() {
		close(stopDrain)
		drainWG.Wait()
	}()

	pub := nodes[0]
	const window = 500 * time.Millisecond
	const settle = 100 * time.Millisecond
	const step = 1000
	const ceilingSafetyBound = 50_000 // guards against a runaway ramp, not a real target

	last := 0
	for rate := 1000; rate <= ceilingSafetyBound; rate += step {
		received.Store(0)
		interval := time.Second / time.Duration(rate)
		deadline := time.Now().Add(window)
		sent := 0
		ticker := time.NewTicker(interval)
		for time.Now().Before(deadline) {
			<-ticker.C
			pub.Publish([]*pb.LogRow{syntheticRow()})
			sent++
		}
		ticker.Stop()
		time.Sleep(settle)

		got := received.Load()
		dropRate := 1 - float64(got)/float64(sent)
		if dropRate > 0.01 {
			return last
		}
		last = rate
	}
	return last
}

// measureBackpressure is the heaviest-weighted measurement per the plan
// doc: does publishing under heavy fan-out load make Node.Publish itself
// (the transport-layer analogue of ingestBatch's RingBuffer.Write +
// Tail.Broadcast call) materially slower than with no subscribers at all?
// A large delta here is disqualifying regardless of latency/throughput.
func measureBackpressure(nodes []*tailfanout.Node) (baseline, underLoad time.Duration) {
	pub := nodes[0]
	const samples = 200

	base := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		pub.Publish([]*pb.LogRow{syntheticRow()})
		base = append(base, time.Since(start))
	}
	baseline = median(base)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Subscribe on every other node without draining — channels saturate
	// at TailBroker's own 1000-row buffer, exercising its drop path, while
	// a concurrent background publisher keeps sustained fan-out pressure
	// on the transport during sampling.
	for _, n := range nodes[1:] {
		n.Subscribe(ctx, fanoutMatcher())
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				pub.Publish([]*pb.LogRow{syntheticRow()})
			}
		}
	}()

	loaded := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		pub.Publish([]*pb.LogRow{syntheticRow()})
		loaded = append(loaded, time.Since(start))
	}
	close(stop)
	wg.Wait()
	underLoad = median(loaded)
	return baseline, underLoad
}

type fanoutResult struct {
	transport                       string
	n                               int
	p50, p95, p99                   time.Duration
	throughput                      int
	ingestBaseline, ingestUnderLoad time.Duration
}

func runFanoutCell(t *testing.T, transport string, n int, nodes []*tailfanout.Node) fanoutResult {
	t.Helper()
	p50, p95, p99 := measureLatency(t, nodes, 5000)
	throughput := measureThroughputCeiling(t, nodes)
	baseline, underLoad := measureBackpressure(nodes)
	return fanoutResult{
		transport:       transport,
		n:               n,
		p50:             p50,
		p95:             p95,
		p99:             p99,
		throughput:      throughput,
		ingestBaseline:  baseline,
		ingestUnderLoad: underLoad,
	}
}

func formatResultsTable(results []fanoutResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-9s %-3s %-9s %-9s %-9s %-16s %-14s %-15s %s\n",
		"transport", "N", "p50", "p95", "p99", "throughput_ceil", "ingest_base", "ingest_loaded", "delta")
	for _, r := range results {
		delta := "n/a"
		if r.ingestBaseline > 0 {
			pct := (float64(r.ingestUnderLoad) - float64(r.ingestBaseline)) / float64(r.ingestBaseline) * 100
			delta = fmt.Sprintf("%+.0f%%", pct)
		}
		fmt.Fprintf(&b, "%-9s %-3d %-9s %-9s %-9s %-16s %-14s %-15s %s\n",
			r.transport, r.n, r.p50, r.p95, r.p99,
			fmt.Sprintf("%d rows/s", r.throughput),
			r.ingestBaseline, r.ingestUnderLoad, delta)
	}
	return b.String()
}

func TestTailFanoutBenchmarkSuite(t *testing.T) {
	if os.Getenv("WEFT_BENCH_TAILFANOUT") != "1" {
		t.Skip("set WEFT_BENCH_TAILFANOUT=1 to run the (slow) tail-fanout transport benchmark suite")
	}

	var mu sync.Mutex
	var results []fanoutResult
	record := func(r fanoutResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	ns := []int{2, 3, 5}

	t.Run("redis", func(t *testing.T) {
		addr := requireRedis(t)
		for _, n := range ns {
			t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
				nodes := newRedisCluster(t, n, addr)
				record(runFanoutCell(t, "redis", n, nodes))
			})
		}
	})

	if len(results) == 0 {
		t.Skip("no fanout cells produced results")
	}
	t.Logf("tail-fanout benchmark results:\n%s", formatResultsTable(results))
}
