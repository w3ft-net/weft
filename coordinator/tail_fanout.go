package coordinator

import (
	"context"
	"sync"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// tailTopic is the routing key for optional cross-coordinator fan-out: the
// structural (non-content) portion of a RowMatcher. Deliberately its own
// copy, not an import of bench/tailfanout.Topic — bench/ is throwaway
// benchmark code (docs/plans/coordinator-horizontal-scaling.md item 4);
// production must not depend on it.
type tailTopic struct {
	Namespace, Service, Kind string
	Wildcard                 bool
}

func topicFor(m *RowMatcher) tailTopic {
	if m.Namespace == "" {
		return tailTopic{Wildcard: true}
	}
	return tailTopic{Namespace: m.Namespace, Service: m.Service, Kind: m.Kind}
}

// TailFanout wraps a *TailBroker with optional Redis-Pub/Sub cross-
// coordinator delivery (docs/plans/coordinator-horizontal-scaling.md item
// 4). With no Redis configured it is a pure passthrough to broker —
// today's exact local-only tail -f behavior, zero Redis dependency in
// dev/tests that don't configure it.
type TailFanout struct {
	broker *TailBroker
	redis  *redisFanout // nil = no cross-coordinator delivery configured

	mu   sync.Mutex
	refs map[tailTopic]int // local-subscriber refcount per topic, drives announceInterest
}

// NewTailFanout wraps broker. redisAddr == "" disables Redis entirely —
// see newRedisFanout for why a non-empty addr never fails construction.
func NewTailFanout(broker *TailBroker, redisAddr string, redisTLS bool) *TailFanout {
	f := &TailFanout{broker: broker, refs: make(map[tailTopic]int)}
	if redisAddr != "" {
		f.redis = newRedisFanout(redisAddr, redisTLS, broker.Broadcast)
	}
	return f
}

// Subscribe registers a local tail subscription and, on the first
// subscriber for a topic, announces interest to Redis.
func (f *TailFanout) Subscribe(ctx context.Context, matcher *RowMatcher) (uint64, <-chan *pb.LogRow) {
	id, ch := f.broker.Subscribe(ctx, matcher)
	if f.redis == nil {
		return id, ch
	}
	t := topicFor(matcher)
	f.mu.Lock()
	f.refs[t]++
	first := f.refs[t] == 1
	f.mu.Unlock()
	if first {
		f.redis.announceInterest(t, true)
	}
	return id, ch
}

// Unsubscribe needs matcher (unlike TailBroker.Unsubscribe(id)) to know
// which topic's refcount to decrement.
func (f *TailFanout) Unsubscribe(id uint64, matcher *RowMatcher) {
	f.broker.Unsubscribe(id)
	if f.redis == nil {
		return
	}
	t := topicFor(matcher)
	f.mu.Lock()
	f.refs[t]--
	last := f.refs[t] <= 0
	if last {
		delete(f.refs, t)
	}
	f.mu.Unlock()
	if last {
		f.redis.announceInterest(t, false)
	}
}

// Publish delivers rows to this process's own local subscribers (mirroring
// ingestBatch's prior direct Tail.Broadcast call) and, if Redis is
// configured, fans them out to interested peer coordinators.
func (f *TailFanout) Publish(rows []*pb.LogRow) {
	f.broker.Broadcast(rows)
	if f.redis != nil {
		f.redis.publish(rows)
	}
}
