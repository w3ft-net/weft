package redisbus

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// requireLocalRedis skips the test if no Redis/Valkey is reachable on the
// docker-compose bench harness's host port — mirrors
// bench/tailfanout/node_test.go's requireLocalRedis, duplicated rather
// than imported since coordinator/ must not depend on bench/.
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

// TestTailFanout_RedisCrossDelivery proves two TailFanout instances (each
// wrapping its own local TailBroker, simulating two coordinator
// processes) deliver rows to each other over Redis, scoped to announced
// topics — the actual production behavior ingestBatch/ExecuteFollow now
// depend on.
func TestTailFanout_RedisCrossDelivery(t *testing.T) {
	addr := requireLocalRedis(t)

	a := coordinator.NewTailFanout(coordinator.NewTailBroker(), New(addr, false))
	b := coordinator.NewTailFanout(coordinator.NewTailBroker(), New(addr, false))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	matcher := &coordinator.RowMatcher{Coords: shape.Set{{Namespace: shape.Value("ns1"), Service: shape.Value("svc1"), Kind: shape.Value("main")}}}
	id, ch := b.Subscribe(ctx, matcher)
	defer b.Unsubscribe(context.Background(), id, matcher)

	time.Sleep(200 * time.Millisecond) // let the PSUBSCRIBE register with redis

	a.Publish([]coordinator.Row{
		{LogRow: &pb.LogRow{Namespace: "ns1", Service: "svc1", Kind: "main", Raw: "match"}},
		{LogRow: &pb.LogRow{Namespace: "ns2", Service: "svc2", Kind: "main", Raw: "no-match"}},
	})

	select {
	case row := <-ch:
		if row.Raw != "match" {
			t.Fatalf("row.Raw = %q, want %q", row.Raw, "match")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cross-delivered row")
	}

	select {
	case row := <-ch:
		t.Fatalf("received unexpected second row: %+v", row)
	case <-time.After(300 * time.Millisecond):
		// expected: the unrelated topic's row never crosses the wire
	}
}
