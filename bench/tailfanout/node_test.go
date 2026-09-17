package tailfanout

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// This is a wire-level sanity check for the Redis candidate's topic
// scoping — proof the benchmark's throughput/latency numbers mean
// something before trusting them. Asserts a row for a topic a peer has no
// interest in never even crosses the transport (checked by instrumenting
// Transport.SetDeliverFunc directly, bypassing coordinator.TailBroker's
// own RowMatcher filtering, which would mask an over-broadcasting
// transport).

func TestRedisTransport_TopicScopedDelivery(t *testing.T) {
	addr := requireLocalRedis(t)

	a, err := NewRedisTransport("a", addr)
	if err != nil {
		t.Fatalf("new redis transport a: %v", err)
	}
	defer a.Close()

	b, err := NewRedisTransport("b", addr)
	if err != nil {
		t.Fatalf("new redis transport b: %v", err)
	}
	defer b.Close()

	var mu sync.Mutex
	var received []*pb.LogRow
	b.SetDeliverFunc(func(rows []*pb.LogRow) {
		mu.Lock()
		received = append(received, rows...)
		mu.Unlock()
	})

	b.AnnounceInterest(Topic{Namespace: "ns1", Service: "svc1", Kind: "main"}, true)
	time.Sleep(200 * time.Millisecond) // let the PSUBSCRIBE register with redis

	a.Publish([]*pb.LogRow{
		{Namespace: "ns1", Service: "svc1", Kind: "main", Raw: "match"},
		{Namespace: "ns2", Service: "svc2", Kind: "main", Raw: "no-match"},
	})
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected exactly 1 row to cross the wire, got %d: %+v", len(received), received)
	}
	if received[0].Raw != "match" {
		t.Fatalf("wrong row delivered: %+v", received[0])
	}
}

func requireLocalRedis(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("WEFT_BENCH_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6380"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable at %s (start it with `docker compose up -d redis`): %v", addr, err)
	}
	return addr
}
