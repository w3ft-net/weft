package coordinator

import (
	"context"
	"sync"
	"sync/atomic"

	weftsdk "github.com/w3ft-net/weft-go"
)

// RingBuffer is a fixed-size circular buffer for log rows. Writes never block;
// if the buffer is full, the oldest rows are overwritten, and every
// overwritten row is counted (Dropped) and logged so the loss is never
// silent. Readers drain batches for shard ingestion.
type RingBuffer struct {
	mu      sync.Mutex
	buf     []Row
	size    int
	head    int           // next write position
	count   int           // number of unread rows (for drain)
	notify  chan struct{} // signaled on write
	dropped atomic.Int64  // rows overwritten before they were drained
}

// Dropped returns how many rows have been overwritten unread since the
// buffer was created.
func (r *RingBuffer) Dropped() int64 { return r.dropped.Load() }

// UsagePct returns the buffer fill percentage (0-100).
func (r *RingBuffer) UsagePct() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return 0
	}
	return r.count * 100 / r.size
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		buf:    make([]Row, capacity),
		size:   capacity,
		notify: make(chan struct{}, 1),
	}
}

// Write appends rows to the buffer. Never blocks.
func (r *RingBuffer) Write(rows []Row) {
	r.mu.Lock()
	overwritten := 0
	for _, row := range rows {
		r.buf[r.head] = row
		r.head = (r.head + 1) % r.size
		if r.count < r.size {
			r.count++
		} else {
			overwritten++
		}
	}
	r.mu.Unlock()
	if overwritten > 0 {
		total := r.dropped.Add(int64(overwritten))
		weftsdk.Errorf(context.Background(), "ingest ring buffer full: overwrote %d undrained rows (%d since start)", overwritten, total)
	}

	// Non-blocking signal to drain goroutine.
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Drain returns up to maxRows unread rows and removes them from the buffer.
// Returns nil if no rows are available.
func (r *RingBuffer) Drain(maxRows int) []Row {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil
	}

	n := r.count
	if n > maxRows {
		n = maxRows
	}

	// Read from tail (oldest unread) forward.
	tail := (r.head - r.count + r.size) % r.size
	rows := make([]Row, n)
	for i := 0; i < n; i++ {
		idx := (tail + i) % r.size
		rows[i] = r.buf[idx]
		// Release the slot: otherwise a drained row stays reachable until
		// the write head laps it, pinning up to capacity rows' worth of heap.
		r.buf[idx] = Row{}
	}
	r.count -= n

	return rows
}

// Notify returns a channel that receives a signal when new rows are available.
func (r *RingBuffer) Notify() <-chan struct{} {
	return r.notify
}
