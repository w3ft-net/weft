package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

var (
	levels    = []string{"INFO", "INFO", "INFO", "WARN", "WARN", "ERROR", "DEBUG"}
	actions   = []string{"request", "query", "auth", "deploy", "batch", "health", "cache", "signup"}
	instances = []string{"i-001", "i-002", "i-003", "i-004"}
)

func runIngest(ctx context.Context, cfg *Config, report *Report) {
	ratePerCustomer := cfg.IngestRate / cfg.Customers
	if ratePerCustomer < 1 {
		ratePerCustomer = 1
	}

	var sent, acked, errors atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < cfg.Customers; i++ {
		wg.Add(1)
		customer := fmt.Sprintf("loadtest-%d", i)
		app := fmt.Sprintf("app-%d", i%3)

		// Each simulated customer needs its own token — IngestStream
		// validates the bearer and attributes rows to the token's customer.
		tokResp, err := cfg.Client.CreateToken(ctx, &pb.CreateTokenRequest{
			Customer:  customer,
			Namespace: app,
			Label:     "loadtest",
		})
		if err != nil {
			errors.Add(1)
			wg.Done()
			continue
		}

		go func(customer, app, token string, rate int) {
			defer wg.Done()
			ingestCustomer(ctx, cfg.Client, customer, app, token, rate, &sent, &acked, &errors)
		}(customer, app, tokResp.Token, ratePerCustomer)
	}

	wg.Wait()

	elapsed := cfg.Duration.Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	report.Ingest = IngestReport{
		RowsSent:  sent.Load(),
		RowsAcked: acked.Load(),
		AvgRate:   float64(sent.Load()) / elapsed,
		Errors:    errors.Load(),
	}
}

func ingestCustomer(ctx context.Context, client pb.WeftCoordinatorClient, customer, app, token string, rate int, sent, acked, errors *atomic.Int64) {
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	stream, err := client.IngestStream(authed)
	if err != nil {
		errors.Add(1)
		return
	}

	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			resp, err := stream.CloseAndRecv()
			if err == nil {
				acked.Add(int64(resp.RowsReceived))
			}
			return
		case <-ticker.C:
			row := generateRow(customer, app)
			if err := stream.Send(row); err != nil {
				errors.Add(1)
				return
			}
			sent.Add(1)
		}
	}
}

func generateRow(customer, app string) *pb.LogRow {
	now := time.Now()
	level := levels[rand.Intn(len(levels))]
	action := actions[rand.Intn(len(actions))]
	instance := instances[rand.Intn(len(instances))]
	status := 200
	if level == "ERROR" {
		status = 500
	}

	raw := fmt.Sprintf("%s %s %s %s %s duration=%dms",
		now.Format("2006-01-02T15:04:05"),
		level, app, instance, action,
		rand.Intn(500))

	payload, _ := json.Marshal(map[string]any{
		"status":      status,
		"action":      action,
		"duration_ms": rand.Intn(500),
	})

	return &pb.LogRow{
		Raw:         raw,
		Service:     app,
		Level:       level,
		TsUnixNs:    now.UnixNano(),
		Instance:    instance,
		Customer:    customer,
		PayloadJson: payload,
		Fields:      []string{now.Format("2006-01-02T15:04:05"), level, app, instance, action},
	}
}
