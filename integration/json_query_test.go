//go:build integration && !localstack

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestDockerStack_JqFieldExtraction reproduces the .pct failure: ingesting
// JSON metrics rows and querying with jq 'select(.type == "memory") | .pct'.
// The pipeline splits into a SQL pushdown (select) and a compute stage (.pct).
func TestDockerStack_JqFieldExtraction(t *testing.T) {
	conn, client := grpcClient(t)
	defer conn.Close()

	// Ingest JSON metrics rows — same shape as weftd metrics collector.
	token := ensureToken(t, client, "testcust", "jqtest")
	stream := openAuthedStream(t, context.Background(), client, token)

	// Per-run app so repeated runs against a long-lived stack don't
	// see each other's metrics rows in the count assertions.
	app := fmt.Sprintf("jqtest/r%d-metrics", time.Now().UnixNano())
	logPath := fmt.Sprintf("/log/%s.log", app)

	now := time.Now()
	metrics := []map[string]any{
		{"type": "memory", "pct": 57.49, "total_mb": 7864, "used_mb": 4521},
		{"type": "cpu", "user": 12.3, "system": 3.1, "idle": 84.6},
		{"type": "memory", "pct": 62.1, "total_mb": 7864, "used_mb": 4883},
		{"type": "disk", "pct": 45.0, "path": "/", "total_gb": 100},
		{"type": "memory", "pct": 31.88, "total_mb": 436, "used_mb": 139},
	}

	for i, m := range metrics {
		payload, _ := json.Marshal(m)
		stream.Send(&pb.LogRow{
			App:         app,
			Customer:    "testcust",
			Raw:         string(payload),
			PayloadJson: payload,
			Level:       "INFO",
			Instance:    "test-host",
			TsUnixNs:    now.Add(time.Duration(i) * time.Second).UnixNano(),
		})
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("close ingest stream: %v", err)
	}
	if resp.RowsReceived != int64(len(metrics)) {
		t.Fatalf("expected %d rows ingested, got %d", len(metrics), resp.RowsReceived)
	}

	// Wait for drain to flush.
	var allRows []*pb.ResultRow
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(2 * time.Second)
		allRows = queryPipeline(t, client, "cat "+logPath, "testcust")
		if len(allRows) >= len(metrics) {
			break
		}
	}
	if len(allRows) == 0 {
		t.Fatal("no rows returned after drain wait")
	}
	t.Logf("ingested and queryable: %d rows", len(allRows))

	// Test 1: jq select alone (full pushdown, no compute) — should work.
	t.Run("jq select only", func(t *testing.T) {
		rows := queryPipeline(t, client,
			fmt.Sprintf(`cat %s | jq 'select(.type == "memory")'`, logPath), "testcust")
		if len(rows) != 3 {
			t.Errorf("expected 3 memory rows, got %d", len(rows))
		}
	})

	// Test 2: jq select + field extraction (partial pushdown + compute).
	// This is the failing pipeline.
	t.Run("jq select and field extraction", func(t *testing.T) {
		text := queryText(t, client,
			fmt.Sprintf(`cat %s | jq 'select(.type == "memory") | .pct'`, logPath), "testcust")
		lines := nonEmpty(strings.Split(strings.TrimSpace(text), "\n"))
		if len(lines) != 3 {
			t.Fatalf("expected 3 pct values, got %d (text=%q)", len(lines), text)
		}
		for _, line := range lines {
			t.Logf("  .pct = %s", line)
		}
	})

	// Test 3: simple field extraction without select (all compute).
	t.Run("jq field extraction only", func(t *testing.T) {
		text := queryText(t, client,
			fmt.Sprintf(`cat %s | jq '.type'`, logPath), "testcust")
		lines := nonEmpty(strings.Split(strings.TrimSpace(text), "\n"))
		if len(lines) != len(metrics) {
			t.Fatalf("expected %d type values, got %d (text=%q)", len(metrics), len(lines), text)
		}
	})
}

// queryText queries a pipeline and returns the text result (from the compute path).
func queryText(t *testing.T, client pb.WeftCoordinatorClient, pipeline, customer string) string {
	t.Helper()
	stream, err := client.Execute(context.Background(), &pb.CoordinatorExecuteRequest{
		Pipeline: pipeline,
		Customer: customer,
	})
	if err != nil {
		t.Fatalf("query %q: %v", pipeline, err)
	}

	var text strings.Builder
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("query %q: recv error: %v", pipeline, err)
		}
		text.WriteString(resp.Text)
		// Also collect any rows as formatted text (fallback).
		for _, row := range resp.Rows {
			fmt.Fprintln(&text, row.Raw)
		}
	}
	return text.String()
}

func nonEmpty(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
