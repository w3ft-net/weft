package coordinator

import (
	"context"
	"sync"
	"sync/atomic"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TailBroker fans out ingested rows to active tail -f subscriptions.
type TailBroker struct {
	mu          sync.RWMutex
	subscribers map[uint64]*tailSubscription
	nextID      atomic.Uint64
}

type tailSubscription struct {
	matcher *RowMatcher
	ch      chan *pb.LogRow
	ctx     context.Context
}

// NewTailBroker creates a broker ready to accept subscriptions.
func NewTailBroker() *TailBroker {
	return &TailBroker{
		subscribers: make(map[uint64]*tailSubscription),
	}
}

// Subscribe registers a new tail subscription. Returns a channel that
// receives matching rows. The subscription is automatically cleaned up
// when the context is cancelled (e.g., client disconnect).
func (b *TailBroker) Subscribe(ctx context.Context, matcher *RowMatcher) (uint64, <-chan *pb.LogRow) {
	id := b.nextID.Add(1)
	ch := make(chan *pb.LogRow, 1000)

	b.mu.Lock()
	b.subscribers[id] = &tailSubscription{
		matcher: matcher,
		ch:      ch,
		ctx:     ctx,
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
	}
	b.mu.Unlock()
}

// HasSubscriber reports whether any active subscription's matcher would
// accept row, without delivering it. Used to decide whether an ingest
// batch containing row is worth flushing early instead of waiting for
// the periodic ticker.
func (b *TailBroker) HasSubscriber(row *pb.LogRow) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
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
func (b *TailBroker) Broadcast(rows []*pb.LogRow) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		if sub.ctx.Err() != nil {
			continue // subscriber disconnected, skip
		}
		for _, row := range rows {
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
