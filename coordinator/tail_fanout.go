package coordinator

import (
	"context"
	"slices"
	"sync"

	"github.com/w3ft-net/weft/shape"
)

// tailTopic is the routing key for optional cross-coordinator fan-out: the
// structural (non-content) portion of a RowMatcher. Deliberately its own
// copy, not an import of bench/tailfanout.Topic — bench/ is throwaway
// benchmark code; production must not depend on it.
type tailTopic struct {
	Added                    shape.Added
	Namespace, Service, Kind string
	Wildcard                 bool
}

// topicsFor is the superset backend over a matcher's coordinates: one topic per
// distinct tuple, as wide as the tuple's namespace, service and kind allow. A
// peer may deliver a row the matcher then rejects (env, say), never the reverse.
func topicsFor(m *RowMatcher) []tailTopic {
	var out []tailTopic
	for _, t := range m.Coords {
		topic := tailTopic{Added: t.Added()}
		if ns, ok := t.Namespace.Literal(); !ok {
			topic.Wildcard = true
		} else {
			topic.Namespace = ns
			topic.Service, _ = t.Service.Literal()
			topic.Kind, _ = t.Kind.Literal()
		}
		if !slices.Contains(out, topic) {
			out = append(out, topic)
		}
	}
	return out
}

// TailFanout wraps a *TailBroker with optional Redis-Pub/Sub cross-
// coordinator delivery. With no Redis configured it is a pure passthrough to broker —
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
func (f *TailFanout) Subscribe(ctx context.Context, matcher *RowMatcher) (uint64, <-chan Row) {
	id, ch := f.broker.Subscribe(ctx, matcher)
	if f.redis == nil {
		return id, ch
	}
	for _, t := range topicsFor(matcher) {
		f.mu.Lock()
		f.refs[t]++
		first := f.refs[t] == 1
		f.mu.Unlock()
		if first {
			f.redis.announceInterest(ctx, t, true)
		}
	}
	return id, ch
}

// Unsubscribe needs matcher (unlike TailBroker.Unsubscribe(id)) to know
// which topic's refcount to decrement.
func (f *TailFanout) Unsubscribe(ctx context.Context, id uint64, matcher *RowMatcher) {
	f.broker.Unsubscribe(id)
	if f.redis == nil {
		return
	}
	for _, t := range topicsFor(matcher) {
		f.mu.Lock()
		f.refs[t]--
		last := f.refs[t] <= 0
		if last {
			delete(f.refs, t)
		}
		f.mu.Unlock()
		if last {
			f.redis.announceInterest(ctx, t, false)
		}
	}
}

// HasSubscriber reports whether row matches a local tail -f subscriber.
// Cross-coordinator subscribers (reachable only via Redis) aren't
// visible here — this is a local-only fast-path signal, not a
// fleet-wide one.
func (f *TailFanout) HasSubscriber(row Row) bool {
	return f.broker.HasSubscriber(row)
}

// Publish delivers rows to this process's own local subscribers (mirroring
// ingestBatch's prior direct Tail.Broadcast call) and, if Redis is
// configured, fans them out to interested peer coordinators.
func (f *TailFanout) Publish(rows []Row) {
	f.broker.Broadcast(rows)
	if f.redis != nil {
		f.redis.publish(rows)
	}
}
