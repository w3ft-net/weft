package bench

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// coordAddr is the coordinator gRPC address for benchmarks.
// Set WEFT_BENCH_ADDR to override (default: localhost:9020).
var coordAddr = "localhost:9020"

func init() {
	if v := os.Getenv("WEFT_BENCH_ADDR"); v != "" {
		coordAddr = v
	}
}

// connect returns a coordinator gRPC client.
func connect(tb testing.TB) pb.WeftCoordinatorClient {
	tb.Helper()
	conn, err := grpc.NewClient(coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		tb.Fatalf("connect to %s: %v", coordAddr, err)
	}
	tb.Cleanup(func() { conn.Close() })
	return pb.NewWeftCoordinatorClient(conn)
}

// ensureData loads N rows into the coordinator via IngestStream if not already loaded.
var dataLoaded atomic.Bool

func ensureData(tb testing.TB, client pb.WeftCoordinatorClient, n int) {
	if dataLoaded.Swap(true) {
		return // already loaded in this process
	}
	tb.Logf("loading %d rows...", n)
	start := time.Now()

	stream, err := client.IngestStream(context.Background())
	if err != nil {
		tb.Fatalf("open ingest stream: %v", err)
	}

	apps := []string{"vimgym", "grantcentral", "callbox", "xylem"}
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	instances := []string{"i-001", "i-002", "i-003", "i-004"}
	actions := []string{
		"request completed", "connection timeout", "health check ok",
		"slow query detected", "cache miss", "user signup completed",
		"deploy started", "authentication failed", "batch job completed",
		"graceful shutdown initiated", "high memory usage",
		"500 internal server error", "connection refused",
		"disk usage above 80%", "cache warmed 2500 keys",
	}

	baseTime := time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewSource(42))

	for i := 0; i < n; i++ {
		app := apps[r.Intn(len(apps))]
		level := levels[r.Intn(len(levels))]
		inst := instances[r.Intn(len(instances))]
		action := actions[r.Intn(len(actions))]
		ts := baseTime.Add(time.Duration(i) * time.Millisecond * 100)
		status := 200 + r.Intn(4)*100 // 200, 300, 400, 500

		raw := fmt.Sprintf("%s %s %s %s %s status=%d",
			ts.Format("2006/01/02 15:04:05"), level, app, inst, action, status)

		row := &pb.LogRow{
			LineNum:  int64(i + 1),
			Raw:      raw,
			App:      app,
			Level:    level,
			Instance: inst,
			Customer: "kevlar",
			TsUnixNs: ts.UnixNano(),
			Fields:   strings.Fields(raw),
		}

		if err := stream.Send(row); err != nil {
			tb.Fatalf("send row %d: %v", i, err)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		tb.Fatalf("close ingest stream: %v", err)
	}
	tb.Logf("loaded %d rows in %v", resp.RowsReceived, time.Since(start))
}

// --- Query benchmarks ---

func query(tb testing.TB, client pb.WeftCoordinatorClient, pipeline string) int {
	tb.Helper()
	stream, err := client.Query(context.Background(), &pb.CoordinatorQueryRequest{
		Pipeline: pipeline,
	})
	if err != nil {
		tb.Fatalf("query %q: %v", pipeline, err)
	}
	total := 0
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		total += len(resp.Rows)
		if resp.Text != "" {
			total++ // count text responses too
		}
	}
	return total
}

func BenchmarkQuery_CatApp(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := query(b, client, "cat vimgym.log")
		if n == 0 {
			b.Fatal("no rows returned")
		}
	}
}

func BenchmarkQuery_CatAll(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := query(b, client, "cat warp.log")
		if n == 0 {
			b.Fatal("no rows returned")
		}
	}
}

func BenchmarkQuery_GrepError(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, "cat vimgym.log | grep ERROR")
	}
}

func BenchmarkQuery_GrepCaseInsensitive(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, "cat warp.log | grep -i timeout")
	}
}

func BenchmarkQuery_AwkLevelFilter(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, `cat vimgym.log | awk '$LEVEL == "ERROR"'`)
	}
}

func BenchmarkQuery_HeadLimit(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, "head -50 warp.log")
	}
}

func BenchmarkQuery_TailLimit(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, "tail -50 warp.log")
	}
}

func BenchmarkQuery_GrepPipeline(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, "cat warp.log | grep ERROR | grep -v timeout")
	}
}

func BenchmarkQuery_ComputeSort(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, `cat vimgym.log | awk '$LEVEL == "ERROR"' | sort`)
	}
}

func BenchmarkQuery_ComputeWcl(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, "cat warp.log | grep timeout | wc -l")
	}
}

func BenchmarkQuery_ComputeSortUniqCount(b *testing.B) {
	client := connect(b)
	ensureData(b, client, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query(b, client, `cat warp.log | awk '$LEVEL == "ERROR"' | sort | uniq -c`)
	}
}

// --- Ingestion benchmarks ---

func BenchmarkIngest_10k(b *testing.B)  { benchIngest(b, 10_000) }
func BenchmarkIngest_50k(b *testing.B)  { benchIngest(b, 50_000) }
func BenchmarkIngest_100k(b *testing.B) { benchIngest(b, 100_000) }

func benchIngest(b *testing.B, rows int) {
	client := connect(b)
	baseTime := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewSource(99))

	// Pre-build rows to exclude generation from timing.
	prebuilt := make([]*pb.LogRow, rows)
	for i := range prebuilt {
		ts := baseTime.Add(time.Duration(i) * time.Millisecond)
		prebuilt[i] = &pb.LogRow{
			LineNum:  int64(i),
			Raw:      fmt.Sprintf("%s INFO bench i-001 test row %d", ts.Format("2006/01/02 15:04:05"), r.Int()),
			App:      "bench",
			Level:    "INFO",
			Instance: "i-001",
			Customer: "kevlar",
			TsUnixNs: ts.UnixNano(),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream, err := client.IngestStream(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		for _, row := range prebuilt {
			if err := stream.Send(row); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := stream.CloseAndRecv(); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Tail fan-out benchmarks ---

func BenchmarkTailFanout_1sub(b *testing.B)   { benchTailFanout(b, 1) }
func BenchmarkTailFanout_10sub(b *testing.B)  { benchTailFanout(b, 10) }
func BenchmarkTailFanout_50sub(b *testing.B)  { benchTailFanout(b, 50) }
func BenchmarkTailFanout_100sub(b *testing.B) { benchTailFanout(b, 100) }

// tailSubscription defines a heterogeneous tail subscriber for benchmarking.
var tailPipelines = []string{
	"tail -f vimgym.log",
	"tail -f grantcentral.log",
	"tail -f callbox.log",
	"tail -f warp.log",                          // all apps
	"tail -f vimgym.log | grep ERROR",
	"tail -f warp.log | grep timeout",
	"tail -f grantcentral.log | grep -v health",
	"tail -f callbox.log | grep -i auth",
}

func benchTailFanout(b *testing.B, numSubscribers int) {
	client := connect(b)
	ensureData(b, client, 10_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		// Open N tail -f subscribers with varied pipelines.
		var wg sync.WaitGroup
		var totalRows atomic.Int64

		for s := 0; s < numSubscribers; s++ {
			wg.Add(1)
			pipeline := tailPipelines[s%len(tailPipelines)]
			go func() {
				defer wg.Done()
				stream, err := client.Query(ctx, &pb.CoordinatorQueryRequest{
					Pipeline: pipeline,
					Follow:   true,
				})
				if err != nil {
					return
				}
				for {
					resp, err := stream.Recv()
					if err != nil {
						return
					}
					totalRows.Add(int64(len(resp.Rows)))
				}
			}()
		}

		// Give subscribers time to connect, then push rows for multiple apps.
		time.Sleep(100 * time.Millisecond)

		ingestStream, err := client.IngestStream(ctx)
		if err != nil {
			cancel()
			b.Fatal(err)
		}

		apps := []string{"vimgym", "grantcentral", "callbox", "xylem"}
		levels := []string{"INFO", "ERROR", "WARN", "DEBUG"}
		actions := []string{
			"request completed", "connection timeout", "health check ok",
			"authentication failed", "batch job completed",
		}

		const ingestCount = 1000
		baseTime := time.Now()
		r := rand.New(rand.NewSource(int64(i)))
		for j := 0; j < ingestCount; j++ {
			app := apps[r.Intn(len(apps))]
			level := levels[r.Intn(len(levels))]
			action := actions[r.Intn(len(actions))]
			ts := baseTime.Add(time.Duration(j) * time.Millisecond)
			ingestStream.Send(&pb.LogRow{
				LineNum:  int64(j),
				Raw:      fmt.Sprintf("%s %s %s i-001 %s", ts.Format("2006/01/02 15:04:05"), level, app, action),
				App:      app,
				Level:    level,
				Instance: "i-001",
				Customer: "kevlar",
				TsUnixNs: ts.UnixNano(),
			})
		}
		ingestStream.CloseAndRecv()

		// Wait for rows to propagate, then cancel.
		time.Sleep(500 * time.Millisecond)
		cancel()
		wg.Wait()

		b.ReportMetric(float64(totalRows.Load())/float64(numSubscribers), "rows/subscriber")
	}
}

// --- Helpers ---

// dockerComposeUp starts the docker-compose stack if not already running.
// Call from TestMain to set up the environment.
func dockerComposeUp(m *testing.M) int {
	// Check if coordinator is already reachable.
	conn, err := grpc.NewClient(coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err == nil {
		client := pb.NewWeftCoordinatorClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := client.ListApps(ctx, &pb.ListAppsRequest{}); err == nil {
			conn.Close()
			fmt.Println("bench: coordinator already running at", coordAddr)
			return m.Run()
		}
		conn.Close()
	}

	fmt.Println("bench: starting docker-compose stack...")
	up := exec.Command("docker", "compose", "up", "-d", "--build", "--wait")
	up.Dir = ".."
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "docker compose up failed: %v\n", err)
		return 1
	}

	// Wait for coordinator to be ready.
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		conn, err := grpc.NewClient(coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			continue
		}
		client := pb.NewWeftCoordinatorClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err = client.ListApps(ctx, &pb.ListAppsRequest{})
		cancel()
		conn.Close()
		if err == nil {
			fmt.Println("bench: coordinator ready")
			break
		}
	}

	code := m.Run()

	// Leave stack running for re-runs. Use `docker compose down` to clean up.
	return code
}

func TestMain(m *testing.M) {
	os.Exit(dockerComposeUp(m))
}
