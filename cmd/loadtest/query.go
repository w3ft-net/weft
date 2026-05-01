package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

var lightQueries = []string{
	"cat app-%d.log | grep ERROR | wc -l",
	"cat app-%d.log | grep INFO | wc -l",
	"cat app-%d.log | head -10",
	"tail app-%d.log | head -5",
}

var heavyQueries = []string{
	"cat app-%d.log | awk '{print $4}' | sort | uniq -c | sort -rn | head -10",
	"cat app-%d.log | grep ERROR | awk '{print $5}' | sort | uniq -c",
	"cat app-%d.log | jq '.status' | sort | uniq -c",
}

func runQueries(ctx context.Context, cfg *Config, report *Report) {
	if cfg.QueryRate <= 0 {
		return
	}

	interval := time.Duration(float64(time.Second) / cfg.QueryRate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var total, errors atomic.Int64
	var mu sync.Mutex
	var latencies []float64

	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			mu.Lock()
			report.Query = buildQueryReport(total.Load(), errors.Load(), latencies)
			mu.Unlock()
			return
		case <-ticker.C:
			wg.Add(1)
			go func() {
				defer wg.Done()
				app := rand.Intn(3)
				var pipeline string
				if rand.Intn(100) < cfg.HeavyPct {
					pipeline = fmt.Sprintf(heavyQueries[rand.Intn(len(heavyQueries))], app)
				} else {
					pipeline = fmt.Sprintf(lightQueries[rand.Intn(len(lightQueries))], app)
				}

				start := time.Now()
				err := execQuery(ctx, cfg.Client, pipeline)
				elapsed := time.Since(start)

				total.Add(1)
				if err != nil {
					errors.Add(1)
				}
				mu.Lock()
				latencies = append(latencies, float64(elapsed.Milliseconds()))
				mu.Unlock()
			}()
		}
	}
}

func execQuery(ctx context.Context, client pb.WeftCoordinatorClient, pipeline string) error {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := client.Execute(queryCtx, &pb.CoordinatorExecuteRequest{Pipeline: pipeline})
	if err != nil {
		return err
	}

	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func buildQueryReport(total, errors int64, latencies []float64) QueryReport {
	r := QueryReport{Total: total, Errors: errors}
	if len(latencies) == 0 {
		return r
	}
	sort.Float64s(latencies)
	r.LatencyP50ms = percentile(latencies, 50)
	r.LatencyP95ms = percentile(latencies, 95)
	r.LatencyP99ms = percentile(latencies, 99)
	return r
}

func percentile(sorted []float64, pct int) float64 {
	idx := pct * len(sorted) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
