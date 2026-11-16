package shard

import (
	"context"
	"path/filepath"
	"testing"
)

// BenchmarkIngestBatch is BenchmarkIngest for a batch that carries its
// partition values, the way the coordinator now sends it: the customer is typed
// once per batch instead of read off every row.
func BenchmarkIngestBatch(b *testing.B) {
	ds, err := NewDuckDBShard("bench", filepath.Join(b.TempDir(), "shard.duckdb"))
	if err != nil {
		b.Fatal(err)
	}
	defer ds.Close()
	batch := Batch{Rows: benchRows(500), PartitionValues: []string{"42"}}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ds.IngestBatch(ctx, batch, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*len(batch.Rows))/b.Elapsed().Seconds(), "rows/s")
}
