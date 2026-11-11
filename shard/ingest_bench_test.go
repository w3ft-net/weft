package shard

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// benchRows is one drain batch's worth of realistic rows: a customer's lines
// for one service, with a JSON payload on every other row.
func benchRows(n int) []*pb.LogRow {
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		r := &pb.LogRow{
			Raw:        fmt.Sprintf("2026-11-11T10:00:00Z INFO request id=%d path=/api/v1/items/%d status=200 dur=%dms", i, i%97, i%250),
			TsUnixNs:   1_700_000_000_000_000_000 + int64(i)*1_000_000,
			Level:      "INFO",
			Namespace:  "acme",
			Service:    "api",
			Kind:       "main",
			Instance:   "i-0123456789abcdef",
			CustomerId: 42,
			Env:        "prod",
			Region:     "us-east-2",
		}
		if i%2 == 0 {
			r.PayloadJson = []byte(fmt.Sprintf(`{"id":%d,"status":200,"path":"/api/v1/items/%d"}`, i, i%97))
		}
		rows[i] = r
	}
	return rows
}

// BenchmarkIngest measures the coordinator's write path into one shard: a
// 500-row batch (the drain's batch size) into a file-backed database, as the
// shard runs in production. Compare before and after the layout change.
func BenchmarkIngest(b *testing.B) {
	ds, err := NewDuckDBShard("bench", filepath.Join(b.TempDir(), "shard.duckdb"))
	if err != nil {
		b.Fatal(err)
	}
	defer ds.Close()
	rows := benchRows(500)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ds.Ingest(ctx, rows, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*len(rows))/b.Elapsed().Seconds(), "rows/s")
}
