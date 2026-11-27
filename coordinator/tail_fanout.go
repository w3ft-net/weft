package coordinator

import (
	"context"
	"slices"
	"sync"

	"github.com/w3ft-net/weft/shape"
)

// TailTopic is the routing key for optional cross-coordinator fan-out: the
// structural (non-content) portion of a RowMatcher. Deliberately its own
// copy, not an import of bench/tailfanout.Topic — bench/ is throwaway
// benchmark code; production must not depend on it.
type TailTopic struct {
	Added                    shape.Added
	Namespace, Service, Kind string
	Wildcard                 bool
}

// topicsFor is the superset backend over a matcher's coordinates: one topic per
// distinct tuple, as wide as the tuple's namespace, service and kind allow. A
// peer may deliver a row the matcher then rejects (env, say), never the reverse.
func topicsFor(m *RowMatcher) []TailTopic {
	var out []TailTopic
	for _, t := range m.Coords {
		topic := TailTopic{Added: t.Added()}
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

// TailBus carries tail rows between coordinators, so a `tail -f` on one sees what
// another ingested. It is optional: with none, tail is local to the process.
// Delivery is best-effort. An implementation (Redis Pub/Sub today, ext/tailfanout/
// redis once moved) names its own transport; the core names none.
type TailBus interface {
	// Start begins receiving rows peers publish for the topics this process has
	// shown interest in, handing them to deliver. It is called once, before use.
	Start(deliver func(rows []Row))
	// Publish sends rows to the peers interested in them.
	Publish(rows []Row)
	// Interest tells the bus this process now wants (add) or no longer wants a
	// topic's rows. The fanout calls it on a topic's first and last subscriber.
	Interest(ctx context.Context, t TailTopic, add bool)
}

// TailFanout wraps a *TailBroker with optional cross-coordinator delivery over a
// TailBus. With no bus it is a pure passthrough to broker: local-only tail -f.
type TailFanout struct {
	broker *TailBroker
	bus    TailBus // nil = no cross-coordinator delivery configured

	mu   sync.Mutex
	refs map[TailTopic]int // local-subscriber refcount per topic, drives Interest
}

// NewTailFanout wraps broker. A nil bus leaves tail local to this process.
func NewTailFanout(broker *TailBroker, bus TailBus) *TailFanout {
	f := &TailFanout{broker: broker, refs: make(map[TailTopic]int), bus: bus}
	if bus != nil {
		bus.Start(broker.Broadcast)
	}
	return f
}

// Subscribe registers a local tail subscription and, on the first
// subscriber for a topic, announces interest to the bus.
func (f *TailFanout) Subscribe(ctx context.Context, matcher *RowMatcher) (uint64, <-chan Row) {
	id, ch := f.broker.Subscribe(ctx, matcher)
	if f.bus == nil {
		return id, ch
	}
	for _, t := range topicsFor(matcher) {
		f.mu.Lock()
		f.refs[t]++
		first := f.refs[t] == 1
		f.mu.Unlock()
		if first {
			f.bus.Interest(ctx, t, true)
		}
	}
	return id, ch
}

// Unsubscribe needs matcher (unlike TailBroker.Unsubscribe(id)) to know
// which topic's refcount to decrement.
func (f *TailFanout) Unsubscribe(ctx context.Context, id uint64, matcher *RowMatcher) {
	f.broker.Unsubscribe(id)
	if f.bus == nil {
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
			f.bus.Interest(ctx, t, false)
		}
	}
}

// HasSubscriber reports whether row matches a local tail -f subscriber.
// Cross-coordinator subscribers (reachable only via the bus) aren't
// visible here — this is a local-only fast-path signal, not a
// fleet-wide one.
func (f *TailFanout) HasSubscriber(row Row) bool {
	return f.broker.HasSubscriber(row)
}

// Publish delivers rows to this process's own local subscribers (mirroring
// ingestBatch's prior direct Tail.Broadcast call) and, if a bus is
// configured, fans them out to interested peer coordinators.
func (f *TailFanout) Publish(rows []Row) {
	f.broker.Broadcast(rows)
	if f.bus != nil {
		f.bus.Publish(rows)
	}
}
