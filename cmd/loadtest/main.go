// Command loadtest simulates multi-tenant ingestion and query load against
// a running weft stack. It identifies bottlenecks by escalating load and
// measuring latency, throughput, error rates, and resource consumption.
//
// Usage:
//
//	go run ./cmd/loadtest --coordinator=localhost:9020 --duration=2m
//	go run ./cmd/loadtest --scenario=burst --coordinator=localhost:9020
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func main() {
	coordAddr := flag.String("coordinator", "localhost:9020", "coordinator gRPC address")
	healthURL := flag.String("health", "http://localhost:9021/health", "coordinator health endpoint")
	customers := flag.Int("customers", 5, "number of simulated customers")
	ingestRate := flag.Int("ingest-rate", 500, "total rows/second across all customers")
	queryRate := flag.Float64("query-rate", 5, "queries/second")
	heavyPct := flag.Int("heavy-query-pct", 10, "% of queries that are complex pipelines")
	watches := flag.Int("watches", 0, "number of simulated cron watches (query every 60s)")
	tails := flag.Int("tails", 0, "number of concurrent tail -f subscribers")
	duration := flag.Duration("duration", 2*time.Minute, "test duration")
	scenario := flag.String("scenario", "", "run a predefined scenario (baseline, ingest, mixed, watches, burst)")
	reportFile := flag.String("report", "", "write JSON report to file")
	flag.Parse()

	// Connect to coordinator.
	conn, err := grpc.NewClient(*coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	defer conn.Close()
	client := pb.NewWeftCoordinatorClient(conn)

	// If a scenario is specified, override flags.
	if *scenario != "" {
		applyScenario(*scenario, customers, ingestRate, queryRate, heavyPct, watches, tails, duration)
	}

	cfg := &Config{
		Client:      client,
		HealthURL:   *healthURL,
		Customers:   *customers,
		IngestRate:  *ingestRate,
		QueryRate:   *queryRate,
		HeavyPct:    *heavyPct,
		Watches:     *watches,
		Tails:       *tails,
		Duration:    *duration,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration+10*time.Second)
	defer cancel()

	// Handle Ctrl+C.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Fprintf(os.Stderr, "\ninterrupted — collecting results...\n")
		cancel()
	}()

	report := run(ctx, cfg)

	// Print summary.
	printSummary(report)

	if *reportFile != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		os.WriteFile(*reportFile, data, 0644)
		fmt.Fprintf(os.Stderr, "report written to %s\n", *reportFile)
	}
}

// Config holds all load test parameters.
type Config struct {
	Client     pb.WeftCoordinatorClient
	HealthURL  string
	Customers  int
	IngestRate int     // total rows/second
	QueryRate  float64 // queries/second
	HeavyPct   int     // % heavy queries
	Watches    int
	Tails      int
	Duration   time.Duration
}

// Report holds the load test results.
type Report struct {
	Duration     time.Duration          `json:"duration"`
	Ingest       IngestReport           `json:"ingest"`
	Query        QueryReport            `json:"query"`
	Health       []HealthSnapshot       `json:"health"`
	Errors       []string               `json:"errors,omitempty"`
}

type IngestReport struct {
	RowsSent     int64         `json:"rows_sent"`
	RowsAcked    int64         `json:"rows_acked"`
	AvgRate      float64       `json:"avg_rate_rows_per_sec"`
	Errors       int64         `json:"errors"`
}

type QueryReport struct {
	Total        int64         `json:"total"`
	Errors       int64         `json:"errors"`
	LatencyP50ms float64       `json:"latency_p50_ms"`
	LatencyP95ms float64       `json:"latency_p95_ms"`
	LatencyP99ms float64       `json:"latency_p99_ms"`
}

type HealthSnapshot struct {
	Timestamp  time.Time      `json:"timestamp"`
	Goroutines int            `json:"goroutines"`
	HeapMB     int            `json:"heap_mb"`
	Shards     int            `json:"shards"`
}

func run(ctx context.Context, cfg *Config) *Report {
	report := &Report{}
	start := time.Now()

	var wg sync.WaitGroup

	// Health monitor — poll every 2 seconds.
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitorHealth(ctx, cfg.HealthURL, report)
	}()

	// Ingest streams.
	if cfg.IngestRate > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runIngest(ctx, cfg, report)
		}()
	}

	// Query load.
	if cfg.QueryRate > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runQueries(ctx, cfg, report)
		}()
	}

	// Simulated watches.
	if cfg.Watches > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWatches(ctx, cfg, report)
		}()
	}

	// Tail subscribers.
	if cfg.Tails > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTails(ctx, cfg, report)
		}()
	}

	// Wait for duration or interrupt.
	select {
	case <-ctx.Done():
	case <-time.After(cfg.Duration):
	}

	wg.Wait()
	report.Duration = time.Since(start)
	return report
}

func monitorHealth(ctx context.Context, url string, report *Report) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := http.Get(url)
			if err != nil {
				continue
			}
			var data map[string]any
			json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()

			snap := HealthSnapshot{Timestamp: time.Now()}
			if sys, ok := data["system"].(map[string]any); ok {
				if v, ok := sys["goroutines"].(float64); ok {
					snap.Goroutines = int(v)
				}
				if v, ok := sys["heap_mb"].(float64); ok {
					snap.HeapMB = int(v)
				}
			}
			if v, ok := data["shards"].(float64); ok {
				snap.Shards = int(v)
			}

			report.Health = append(report.Health, snap)

			// Live output.
			fmt.Fprintf(os.Stderr, "\r[health] goroutines=%d heap=%dMB shards=%d  ",
				snap.Goroutines, snap.HeapMB, snap.Shards)
		}
	}
}

func printSummary(r *Report) {
	fmt.Fprintf(os.Stderr, "\n\n")
	fmt.Fprintf(os.Stderr, "══ Load Test Results (%s) ══\n\n", r.Duration.Round(time.Second))

	fmt.Fprintf(os.Stderr, "  Ingest:\n")
	fmt.Fprintf(os.Stderr, "    rows sent:  %d\n", r.Ingest.RowsSent)
	fmt.Fprintf(os.Stderr, "    rows acked: %d\n", r.Ingest.RowsAcked)
	fmt.Fprintf(os.Stderr, "    avg rate:   %.0f rows/s\n", r.Ingest.AvgRate)
	fmt.Fprintf(os.Stderr, "    errors:     %d\n", r.Ingest.Errors)

	fmt.Fprintf(os.Stderr, "\n  Queries:\n")
	fmt.Fprintf(os.Stderr, "    total:   %d\n", r.Query.Total)
	fmt.Fprintf(os.Stderr, "    errors:  %d\n", r.Query.Errors)
	fmt.Fprintf(os.Stderr, "    p50:     %.1fms\n", r.Query.LatencyP50ms)
	fmt.Fprintf(os.Stderr, "    p95:     %.1fms\n", r.Query.LatencyP95ms)
	fmt.Fprintf(os.Stderr, "    p99:     %.1fms\n", r.Query.LatencyP99ms)

	if len(r.Health) > 0 {
		last := r.Health[len(r.Health)-1]
		peak := 0
		for _, h := range r.Health {
			if h.HeapMB > peak {
				peak = h.HeapMB
			}
		}
		fmt.Fprintf(os.Stderr, "\n  Coordinator:\n")
		fmt.Fprintf(os.Stderr, "    final goroutines: %d\n", last.Goroutines)
		fmt.Fprintf(os.Stderr, "    peak heap:        %dMB\n", peak)
	}

	if len(r.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "\n  Errors:\n")
		for _, e := range r.Errors {
			fmt.Fprintf(os.Stderr, "    - %s\n", e)
		}
	}
	fmt.Fprintf(os.Stderr, "\n")
}

func applyScenario(name string, customers, ingestRate *int, queryRate *float64, heavyPct, watches, tails *int, duration *time.Duration) {
	switch name {
	case "baseline":
		*customers = 1
		*ingestRate = 100
		*queryRate = 1
		*heavyPct = 0
		*watches = 0
		*tails = 0
		*duration = 2 * time.Minute
	case "ingest":
		*customers = 10
		*ingestRate = 5000
		*queryRate = 0
		*heavyPct = 0
		*watches = 0
		*tails = 0
		*duration = 3 * time.Minute
	case "mixed":
		*customers = 10
		*ingestRate = 1000
		*queryRate = 10
		*heavyPct = 20
		*watches = 10
		*tails = 3
		*duration = 5 * time.Minute
	case "watches":
		*customers = 5
		*ingestRate = 500
		*queryRate = 0
		*heavyPct = 0
		*watches = 100
		*tails = 0
		*duration = 3 * time.Minute
	case "burst":
		*customers = 5
		*ingestRate = 10000
		*queryRate = 20
		*heavyPct = 30
		*watches = 0
		*tails = 0
		*duration = 1 * time.Minute
	default:
		log.Fatalf("unknown scenario: %s (use: baseline, ingest, mixed, watches, burst)", name)
	}
	log.Printf("scenario %s: %d customers, %d rows/s, %.0f queries/s, %d watches, %s",
		name, *customers, *ingestRate, *queryRate, *watches, *duration)
}
