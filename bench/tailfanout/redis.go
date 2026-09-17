package tailfanout

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// redisTransport is the Redis Pub/Sub Transport candidate. Every topic a
// node has local interest in becomes one PSUBSCRIBE pattern; Publish
// resolves each row's (namespace, service, kind) to a concrete channel
// name and PUBLISHes the row's marshaled bytes to it.
//
// Known gap, documented not solved (mirrors the channel-metacharacter gap
// noted on pattern()): if a single node ever holds two *overlapping*
// pattern subscriptions at once (e.g. both the namespace-wide "tail:ns:*"
// and a narrower "tail:ns:svc:*"), Redis delivers a matching message once
// per matching pattern subscription, so deliver() would be invoked twice
// for that row. The benchmark's own subscription scenarios never exercise
// overlapping patterns on the same node, so this is left as a known
// limitation rather than solved with subscription-side dedup.
type redisTransport struct {
	id     string
	client *redis.Client
	ps     *redis.PubSub

	deliverMu sync.RWMutex
	deliver   func(rows []*pb.LogRow)

	mu       sync.Mutex
	patterns map[Topic]string

	done chan struct{}
}

// NewRedisTransport connects to a real redis-server (or Valkey, same wire
// protocol) at addr and returns a Transport ready to be wired into a Node.
func NewRedisTransport(id, addr string) (*redisTransport, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("ping redis at %s: %w", addr, err)
	}

	ps := client.PSubscribe(context.Background()) // patterns added via AnnounceInterest
	r := &redisTransport{
		id:       id,
		client:   client,
		ps:       ps,
		patterns: make(map[Topic]string),
		done:     make(chan struct{}),
	}
	go r.recvLoop()
	return r, nil
}

func (r *redisTransport) recvLoop() {
	defer close(r.done)
	for msg := range r.ps.Channel() {
		row := &pb.LogRow{}
		if err := proto.Unmarshal([]byte(msg.Payload), row); err != nil {
			log.Printf("tailfanout redis %s: unmarshal row: %v", r.id, err)
			continue
		}
		r.deliverMu.RLock()
		deliver := r.deliver
		r.deliverMu.RUnlock()
		if deliver != nil {
			deliver([]*pb.LogRow{row})
		}
	}
}

// pattern builds the PSUBSCRIBE glob for a topic. Always goes through
// PSUBSCRIBE, even for a fully-specified topic, to keep one code path
// instead of branching between SUBSCRIBE/PSUBSCRIBE — a deliberate
// benchmark-harness simplification, not a proposal for how a production
// version would necessarily do it.
//
// Known gap, documented not solved: namespace/service/kind values
// containing Redis glob metacharacters (*, ?, [) would need escaping in a
// production version. The benchmark's synthetic topic values never
// contain them.
func pattern(t Topic) string {
	switch {
	case t.Wildcard:
		return "tail:*"
	case t.Service == "":
		return fmt.Sprintf("tail:%s:*", t.Namespace)
	case t.Kind == "":
		return fmt.Sprintf("tail:%s:%s:*", t.Namespace, t.Service)
	default:
		return fmt.Sprintf("tail:%s:%s:%s*", t.Namespace, t.Service, t.Kind)
	}
}

// channelFor is the concrete publish-side channel name for a row. Kind
// resolves to its "main" default, matching the proto's own documented
// default (proto/weftpb/weft.proto's LogRow.kind comment).
func channelFor(row *pb.LogRow) string {
	kind := row.Kind
	if kind == "" {
		kind = "main"
	}
	return fmt.Sprintf("tail:%s:%s:%s", row.Namespace, row.Service, kind)
}

// AnnounceInterest implements Transport.
func (r *redisTransport) AnnounceInterest(t Topic, add bool) {
	p := pattern(t)
	ctx := context.Background()
	if add {
		r.mu.Lock()
		r.patterns[t] = p
		r.mu.Unlock()
		if err := r.ps.PSubscribe(ctx, p); err != nil {
			log.Printf("tailfanout redis %s: psubscribe %s: %v", r.id, p, err)
		}
		return
	}
	r.mu.Lock()
	delete(r.patterns, t)
	r.mu.Unlock()
	if err := r.ps.PUnsubscribe(ctx, p); err != nil {
		log.Printf("tailfanout redis %s: punsubscribe %s: %v", r.id, p, err)
	}
}

// Publish implements Transport: pipelines one PUBLISH per row so a
// multi-row batch costs one Redis round trip.
func (r *redisTransport) Publish(rows []*pb.LogRow) {
	if len(rows) == 0 {
		return
	}
	ctx := context.Background()
	pipe := r.client.Pipeline()
	for _, row := range rows {
		data, err := proto.Marshal(row)
		if err != nil {
			log.Printf("tailfanout redis %s: marshal row: %v", r.id, err)
			continue
		}
		pipe.Publish(ctx, channelFor(row), data)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("tailfanout redis %s: publish: %v", r.id, err)
	}
}

// SetDeliverFunc implements Transport.
func (r *redisTransport) SetDeliverFunc(f func(rows []*pb.LogRow)) {
	r.deliverMu.Lock()
	r.deliver = f
	r.deliverMu.Unlock()
}

// Close implements Transport.
func (r *redisTransport) Close() error {
	if err := r.ps.Close(); err != nil {
		return err
	}
	<-r.done
	return r.client.Close()
}
