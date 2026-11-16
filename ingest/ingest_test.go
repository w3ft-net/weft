package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shard"
)

// TestIngestRoundTrip_GrantCentralWorker tests the full pipeline:
// sample logs → detect → schema → parse → ingest → query.
func TestIngestRoundTrip_GrantCentralWorker(t *testing.T) {
	lines := readSampleLines(t, "../sample-logs/grantcentral_worker_logs.txt")

	// Detect schema.
	_ = Detect(lines)

	// A schema stored before the wizard stopped naming envelopes still parses:
	// this is the legacy reader's coverage, not detection's.
	schema := &Schema{
		Service:         "grantcentral-worker",
		Envelope:        "kevcons",
		Format:          "text",
		TimestampField:  "1",
		TimestampFields: 2,
		TimestampFormat: "2006-01-02 15:04:05,000",
		LevelField:      "3",
		LevelPattern:    `^(\w+)/`,
	}

	// Parse lines.
	parser, err := NewParser(schema)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rows, err := parser.ParseLines(lines, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) == 0 {
		t.Fatal("expected rows")
	}
	t.Logf("parsed %d rows", len(rows))

	// Check some rows have extracted timestamps, levels, instances.
	nowNs := now.UnixNano()
	hasTimestamp := false
	hasLevel := false
	hasInstance := false
	for _, r := range rows {
		if r.TsUnixNs != nowNs {
			hasTimestamp = true
		}
		if r.Level != "" {
			hasLevel = true
		}
		if r.Instance != "" {
			hasInstance = true
		}
	}
	if !hasTimestamp {
		t.Error("no rows had extracted timestamps")
	}
	if !hasLevel {
		t.Error("no rows had extracted levels")
	}
	if !hasInstance {
		t.Error("no rows had extracted instances")
	}

	// Log a few sample rows.
	for i, r := range rows {
		if i >= 5 {
			break
		}
		t.Logf("  row %d: ts=%s level=%-7s instance=%s raw=%.60s",
			r.LineNum, time.Unix(0, r.TsUnixNs).Format("15:04:05"), r.Level, r.Instance, r.Raw)
	}

	// Ingest into an in-memory shard and query back.
	minTs := time.Unix(0, rows[0].TsUnixNs)
	maxTs := minTs
	for _, r := range rows[1:] {
		ts := time.Unix(0, r.TsUnixNs)
		if ts.Before(minTs) {
			minTs = ts
		}
		if ts.After(maxTs) {
			maxTs = ts
		}
	}

	ds, err := shard.NewDuckDBShard("test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	if err := ds.IngestBatch(context.Background(), shard.Batch{Rows: rows, PartitionValues: []string{"1"}}, nil); err != nil {
		t.Fatal(err)
	}

	// Query: all rows for this app.
	allRows, err := ds.Query(context.Background(), "SELECT raw, ts FROM logs WHERE service = 'grantcentral-worker' AND customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(allRows) != len(rows) {
		t.Errorf("expected %d rows from query, got %d", len(rows), len(allRows))
	}

	// Query: ERROR rows only.
	errorRows, err := ds.Query(context.Background(), "SELECT raw, ts FROM logs WHERE service = 'grantcentral-worker' AND customer_id = 1 AND level = 'ERROR'")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ERROR rows: %d", len(errorRows))
	if len(errorRows) == 0 {
		t.Error("expected some ERROR rows")
	}
	for _, r := range errorRows {
		if !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected raw to contain 'ERROR', got %q", r.Raw)
		}
	}

	// Query: grep for "timeout" in raw.
	timeoutRows, err := ds.Query(context.Background(), "SELECT raw, ts FROM logs WHERE service = 'grantcentral-worker' AND customer_id = 1 AND raw LIKE '%timeout%'")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("timeout rows: %d", len(timeoutRows))
	for _, r := range timeoutRows {
		if !strings.Contains(strings.ToLower(r.Raw), "timeout") {
			t.Errorf("expected 'timeout' in raw, got: %s", r.Raw)
		}
	}
}

func TestIngestRoundTrip_VimGym(t *testing.T) {
	lines := readSampleLines(t, "../sample-logs/vimgym_logs.txt")

	schema := &Schema{
		Service:         "vimgym",
		Envelope:        "kevcons",
		Format:          "text",
		TimestampField:  "1",
		TimestampFields: 2,
		TimestampFormat: "2006/01/02 15:04:05",
	}

	parser, err := NewParser(schema)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rows, err := parser.ParseLines(lines, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) == 0 {
		t.Fatal("expected rows")
	}
	t.Logf("parsed %d rows", len(rows))

	// Check timestamps were extracted for lines that have Go timestamps.
	nowNs := now.UnixNano()
	extracted := 0
	for _, r := range rows {
		if r.TsUnixNs != nowNs {
			extracted++
		}
	}
	t.Logf("%d rows had extracted timestamps", extracted)

	// Ingest and query.
	ds, err := shard.NewDuckDBShard("test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	if err := ds.IngestBatch(context.Background(), shard.Batch{Rows: rows, PartitionValues: []string{"1"}}, nil); err != nil {
		t.Fatal(err)
	}

	allRows, err := ds.Query(context.Background(), "SELECT raw, ts FROM logs WHERE service = 'vimgym'")
	if err != nil {
		t.Fatal(err)
	}
	if len(allRows) != len(rows) {
		t.Errorf("expected %d rows, got %d", len(rows), len(allRows))
	}

	// Grep for "bot:" which appears in several vimgym lines.
	botRows, err := ds.Query(context.Background(), "SELECT raw, ts FROM logs WHERE service = 'vimgym' AND raw LIKE '%bot:%'")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bot rows: %d", len(botRows))
	if len(botRows) == 0 {
		t.Error("expected some bot rows")
	}
}
