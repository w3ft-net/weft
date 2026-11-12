package coordinator

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/graybeardcorp/weft/shape"
)

// TailBroker fans out ingested rows to active tail -f subscriptions.
//
// Every ingested row is offered to the subscriptions, and nearly all of them
// belong to someone else, so subscriptions are bucketed by the customers their
// coordinates name: a row visits only the bucket of its own customer. The
// customer is a required column of every tuple, so no subscription that could
// admit a row is outside its bucket.
type TailBroker struct {
	mu          sync.RWMutex
	subscribers map[uint64]*tailSubscription
	byPartition map[shape.Added][]*tailSubscription
	nextID      atomic.Uint64
}

type tailSubscription struct {
	id         uint64
	matcher    *RowMatcher
	ch         chan Row
	ctx        context.Context
	partitions []shape.Added // the distinct partitions matcher.Coords names
}

// NewTailBroker creates a broker ready to accept subscriptions.
func NewTailBroker() *TailBroker {
	return &TailBroker{
		subscribers: make(map[uint64]*tailSubscription),
		byPartition: make(map[shape.Added][]*tailSubscription),
	}
}

// partitionsOf is the distinct partitions a set names.
func partitionsOf(s shape.Set) []shape.Added {
	var out []shape.Added
	for i := range s {
		if a := s[i].Added(); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// Subscribe registers a new tail subscription. Returns a channel that
// receives matching rows. The subscription is automatically cleaned up
// when the context is cancelled (e.g., client disconnect).
func (b *TailBroker) Subscribe(ctx context.Context, matcher *RowMatcher) (uint64, <-chan Row) {
	id := b.nextID.Add(1)
	ch := make(chan Row, 1000)

	sub := &tailSubscription{id: id, matcher: matcher, ch: ch, ctx: ctx, partitions: partitionsOf(matcher.Coords)}
	b.mu.Lock()
	b.subscribers[id] = sub
	for _, c := range sub.partitions {
		b.byPartition[c] = append(b.byPartition[c], sub)
	}
	b.mu.Unlock()

	return id, ch
}

// Unsubscribe removes a subscription and closes its channel.
func (b *TailBroker) Unsubscribe(id uint64) {
	b.mu.Lock()
	if sub, ok := b.subscribers[id]; ok {
		close(sub.ch)
		delete(b.subscribers, id)
		for _, c := range sub.partitions {
			b.byPartition[c] = slices.DeleteFunc(b.byPartition[c], func(s *tailSubscription) bool { return s.id == id })
			if len(b.byPartition[c]) == 0 {
				delete(b.byPartition, c)
			}
		}
	}
	b.mu.Unlock()
}

// HasSubscriber reports whether any active subscription's matcher would
// accept row, without delivering it. Used to decide whether an ingest
// batch containing row is worth flushing early instead of waiting for
// the periodic ticker.
func (b *TailBroker) HasSubscriber(row Row) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.byPartition[row.Added] {
		if sub.ctx.Err() != nil {
			continue // subscriber disconnected, skip
		}
		if sub.matcher.Match(row) {
			return true
		}
	}
	return false
}

// Broadcast sends rows to all matching subscribers. Non-blocking:
// if a subscriber's channel is full, rows are dropped (tail -f
// semantics — the terminal can scroll past things).
func (b *TailBroker) Broadcast(rows []Row) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, row := range rows {
		for _, sub := range b.byPartition[row.Added] {
			if sub.ctx.Err() != nil {
				continue // subscriber disconnected, skip
			}
			if sub.matcher.Match(row) {
				select {
				case sub.ch <- row:
				default:
					// channel full, drop
				}
			}
		}
	}
}
