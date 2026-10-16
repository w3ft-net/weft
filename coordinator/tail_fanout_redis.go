package coordinator

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/graybeardcorp/weft/internal/weftlog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
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
	// OnConnect fires every time go-redis (re-)establishes the underlying
	// connection — the first call is the initial connect, every call after
	// that is a reconnect following a drop. There's no separate reconnect
	// callback in the client, so this is the only hook into that signal.
	opts.OnConnect = func(ctx context.Context, cn *redis.Conn) error {
		store.LogTailFanoutEvent(ctx, "connect", nil)
		return nil
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
			weftlog.Infof(context.Background(), "tailfanout: redis unmarshal row: %v", err)
			store.LogTailFanoutEvent(context.Background(), "unmarshal", err)
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
		return fmt.Sprintf("tail:%d:*", t.Customer)
	case t.Service == "":
		return fmt.Sprintf("tail:%d:%s:*", t.Customer, t.Namespace)
	case t.Kind == "":
		return fmt.Sprintf("tail:%d:%s:%s:*", t.Customer, t.Namespace, t.Service)
	default:
		return fmt.Sprintf("tail:%d:%s:%s:%s*", t.Customer, t.Namespace, t.Service, t.Kind)
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
	return fmt.Sprintf("tail:%d:%s:%s:%s", row.CustomerId, row.Namespace, row.Service, kind)
}

func (r *redisFanout) announceInterest(ctx context.Context, t tailTopic, add bool) {
	p := pattern(t)
	if add {
		r.mu.Lock()
		r.patterns[t] = p
		r.mu.Unlock()
		if err := r.ps.PSubscribe(ctx, p); err != nil {
			weftlog.Infof(ctx, "tailfanout: redis psubscribe %s: %v", p, err)
			store.LogTailFanoutEvent(ctx, "psubscribe", err)
		}
		return
	}
	r.mu.Lock()
	delete(r.patterns, t)
	r.mu.Unlock()
	if err := r.ps.PUnsubscribe(ctx, p); err != nil {
		weftlog.Infof(ctx, "tailfanout: redis punsubscribe %s: %v", p, err)
		store.LogTailFanoutEvent(ctx, "punsubscribe", err)
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
			weftlog.Infof(ctx, "tailfanout: redis marshal row: %v", err)
			store.LogTailFanoutEvent(ctx, "marshal", err)
			continue
		}
		pipe.Publish(ctx, channelFor(row), data)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		weftlog.Infof(ctx, "tailfanout: redis publish: %v", err)
		store.LogTailFanoutEvent(ctx, "publish", err)
	}
}
