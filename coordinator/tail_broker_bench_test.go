package coordinator

import (
	"context"
	"testing"
)

// BenchmarkTailBroker offers a 500-row batch from 50 customers to 100
// subscriptions, two per customer, as the ingest path does: HasSubscriber per
// row to decide the early flush, then one Broadcast of the batch. The cost is
// per batch, so it shows what every subscription adds to every ingested row.
func BenchmarkTailBroker(b *testing.B) {
	broker := NewTailBroker()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		_, ch := broker.Subscribe(ctx, tailSet(int64(1+i%50)))
		go func() {
			for range ch {
			}
		}()
	}
	rows := make([]Row, 500)
	for i := range rows {
		rows[i] = tailRow(int64(1+i%50), "2026-11-11T10:00:00Z INFO request path=/api/v1/items status=200")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, r := range rows {
			broker.HasSubscriber(r)
		}
		broker.Broadcast(rows)
	}
	b.ReportMetric(float64(b.N*len(rows))/b.Elapsed().Seconds(), "rows/s")
}
