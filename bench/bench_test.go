package bench

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// benchJWTSecret matches the dev coordinator's JWT_SECRET default so tests
// can sign tokens the coordinator will accept without prearranged
// configuration. Override WEFT_BENCH_JWT_SECRET to test against a
// coordinator running with a custom secret.
var benchJWTSecret = func() string {
	if v := os.Getenv("WEFT_BENCH_JWT_SECRET"); v != "" {
		return v
	}
	return "dev-secret-change-in-production"
}()

// benchJWT returns a signed JWT for a fake verified "kevlar" user. The
// coordinator's auth interceptor validates against benchJWTSecret; the
// handlers derive customer from the claim.
func benchJWT(tb testing.TB) string {
	tb.Helper()
	now := time.Now()
	user := &store.User{
		ID:              "bench-user",
		Email:           "bench@example.com",
		CustomerName:    "kevlar",
		EmailVerifiedAt: &now,
		CreatedAt:       now,
	}
	tok, err := auth.Generate(user, benchJWTSecret)
	if err != nil {
		tb.Fatalf("sign bench JWT: %v", err)
	}
	return tok
}

// coordAddr is the coordinator gRPC address for benchmarks.
// Set WEFT_BENCH_ADDR to override (default: localhost:9020).
var coordAddr = "localhost:9020"

func init() {
	if v := os.Getenv("WEFT_BENCH_ADDR"); v != "" {
		coordAddr = v
	}
}

// connect returns a coordinator gRPC client. For ingest-only paths we'd
// hand over the seeded agent token, but the JWT-gated RPCs (Query,
// ListApps, ...) now require a user JWT. Send the user JWT by default;
// ingest paths don't care which bearer is present since the coordinator's
// IngestStream handler does its own agent-token lookup in metadata.
func connect(tb testing.TB) pb.WeftCoordinatorClient {
	tb.Helper()
	conn, err := grpc.NewClient(coordAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(benchBearer{token: benchJWT(tb)}),
	)
	if err != nil {
		tb.Fatalf("connect to %s: %v", coordAddr, err)
	}
	tb.Cleanup(func() { conn.Close() })
	return pb.NewWeftCoordinatorClient(conn)
}

// connectIngest returns a client authenticated with the dev seed agent
// token (tied to customer "kevlar"). Used for benchmarks that exercise
// IngestStream — the coordinator's IngestStream handler expects a
// bearer token formatted as wft_... and looks it up in the agent-token
// table, so a JWT won't work there.
func connectIngest(tb testing.TB) pb.WeftCoordinatorClient {
	tb.Helper()
	conn, err := grpc.NewClient(coordAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(benchBearer{token: "wft_dev_0000000000000000"}),
	)
	if err != nil {
		tb.Fatalf("connect to %s: %v", coordAddr, err)
	}
	tb.Cleanup(func() { conn.Close() })
	return pb.NewWeftCoordinatorClient(conn)
}

type benchBearer struct{ token string }

func (b benchBearer) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (benchBearer) RequireTransportSecurity() bool { return false }

// ensureData loads N rows into the coordinator via IngestStream if not already loaded.
var dataLoaded atomic.Bool

func ensureData(tb testing.TB, _ pb.WeftCoordinatorClient, n int) {
	if dataLoaded.Swap(true) {
		return // already loaded in this process
	}
	tb.Logf("loading %d rows...", n)
	start := time.Now()

	// IngestStream still validates the agent-token bearer inside the
	// handler — the JWT we'd use for queries won't pass that check.
	// Dial a dedicated client with the seeded agent token just for
	// the ingest round-trip.
	ingestClient := connectIngest(tb)
	stream, err := ingestClient.IngestStream(context.Background())
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
	// IngestStream needs an agent-token bearer, not a user JWT.
	client := connectIngest(b)
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
	ingestClient := connectIngest(b)
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

		ingestStream, err := ingestClient.IngestStream(ctx)
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

// requireCoordinator aborts the benchmark run unless the coordinator is
// already reachable. Benchmarks don't bring up docker themselves —
// bring up the stack first: `docker compose up -d --build`.
func requireCoordinator() int {
	conn, err := grpc.NewClient(coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err == nil {
		client := pb.NewWeftCoordinatorClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// Authenticate is in the auth-skip list (agents bootstrap here
		// with their own token in the request body), so we can use it
		// as a cheap liveness probe without needing a signed JWT.
		// An invalid-token reply still tells us the service is alive.
		_, err := client.Authenticate(ctx, &pb.AuthenticateRequest{Token: "probe"})
		if err == nil || strings.Contains(err.Error(), "invalid or revoked") {
			conn.Close()
			fmt.Println("bench: coordinator reachable at", coordAddr)
			return 0
		}
		conn.Close()
	}
	fmt.Fprintf(os.Stderr,
		"bench: coordinator at %s is not reachable. "+
			"Start the stack first: `docker compose up -d --build`.\n",
		coordAddr,
	)
	return 1
}

func TestMain(m *testing.M) {
	if rc := requireCoordinator(); rc != 0 {
		os.Exit(rc)
	}
	os.Exit(m.Run())
}
