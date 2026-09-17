package coordinator

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// redisFanout is TailFanout's optional Redis Pub/Sub piece. Topic-scoped
// PSUBSCRIBE/channel-naming is unchanged from the validated
// bench/tailfanout candidate (docs/plans/coordinator-horizontal-scaling.md
// item 4).
//
// Known gaps, carried over from the bench source as documented-not-solved
// rather than regressions introduced here: a node holding two overlapping
// pattern subscriptions at once would receive a matching row once per
// matching pattern (Redis's own PSUBSCRIBE semantics); namespace/service/
// kind values containing Redis glob metacharacters (*, ?, [) aren't
// escaped.
type redisFanout struct {
	client *redis.Client
	ps     *redis.PubSub

	mu       sync.Mutex
	patterns map[tailTopic]string
}

// newRedisFanout deliberately does not Ping at construction: tail is
// already documented as best-effort, so a transient ElastiCache blip at
// coordinator boot must not crash-loop the process that also owns the
// durable ingest path. go-redis's client is lazy/auto-reconnecting.
func newRedisFanout(addr string, tlsEnabled bool, deliver func(rows []*pb.LogRow)) *redisFanout {
	opts := &redis.Options{Addr: addr}
	if tlsEnabled {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redis.NewClient(opts)
	ps := client.PSubscribe(context.Background()) // patterns added via announceInterest
	r := &redisFanout{client: client, ps: ps, patterns: make(map[tailTopic]string)}
	go r.recvLoop(deliver)
	return r
}

func (r *redisFanout) recvLoop(deliver func(rows []*pb.LogRow)) {
	for msg := range r.ps.Channel() {
		row := &pb.LogRow{}
		if err := proto.Unmarshal([]byte(msg.Payload), row); err != nil {
			log.Printf("tailfanout: redis unmarshal row: %v", err)
			continue
		}
		deliver([]*pb.LogRow{row})
	}
}

// pattern builds the PSUBSCRIBE glob for a topic. Always goes through
// PSUBSCRIBE, even for a fully-specified topic, to keep one code path
// instead of branching between SUBSCRIBE/PSUBSCRIBE.
func pattern(t tailTopic) string {
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

func (r *redisFanout) announceInterest(t tailTopic, add bool) {
	p := pattern(t)
	ctx := context.Background()
	if add {
		r.mu.Lock()
		r.patterns[t] = p
		r.mu.Unlock()
		if err := r.ps.PSubscribe(ctx, p); err != nil {
			log.Printf("tailfanout: redis psubscribe %s: %v", p, err)
		}
		return
	}
	r.mu.Lock()
	delete(r.patterns, t)
	r.mu.Unlock()
	if err := r.ps.PUnsubscribe(ctx, p); err != nil {
		log.Printf("tailfanout: redis punsubscribe %s: %v", p, err)
	}
}

// publish pipelines one PUBLISH per row so a multi-row batch costs one
// Redis round trip.
func (r *redisFanout) publish(rows []*pb.LogRow) {
	if len(rows) == 0 {
		return
	}
	ctx := context.Background()
	pipe := r.client.Pipeline()
	for _, row := range rows {
		data, err := proto.Marshal(row)
		if err != nil {
			log.Printf("tailfanout: redis marshal row: %v", err)
			continue
		}
		pipe.Publish(ctx, channelFor(row), data)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("tailfanout: redis publish: %v", err)
	}
}
