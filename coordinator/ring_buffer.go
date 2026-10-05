package coordinator

import (
	"sync"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// RingBuffer is a fixed-size circular buffer for log rows. Writes never block;
// if the buffer is full, the oldest rows are silently overwritten. Readers
// drain batches for shard ingestion.
type RingBuffer struct {
	mu     sync.Mutex
	buf    []*pb.LogRow
	size   int
	head   int           // next write position
	count  int           // number of unread rows (for drain)
	notify chan struct{} // signaled on write
}

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
		buf:    make([]*pb.LogRow, capacity),
		size:   capacity,
		notify: make(chan struct{}, 1),
	}
}

// Write appends rows to the buffer. Never blocks.
func (r *RingBuffer) Write(rows []*pb.LogRow) {
	r.mu.Lock()
	for _, row := range rows {
		r.buf[r.head] = row
		r.head = (r.head + 1) % r.size
		if r.count < r.size {
			r.count++
		}
	}
	r.mu.Unlock()

	// Non-blocking signal to drain goroutine.
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Drain returns up to maxRows unread rows and removes them from the buffer.
// Returns nil if no rows are available.
func (r *RingBuffer) Drain(maxRows int) []*pb.LogRow {
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
	rows := make([]*pb.LogRow, n)
	for i := 0; i < n; i++ {
		idx := (tail + i) % r.size
		rows[i] = r.buf[idx]
		// Release the slot: otherwise a drained row stays reachable until
		// the write head laps it, pinning up to capacity rows' worth of heap.
		r.buf[idx] = nil
	}
	r.count -= n

	return rows
}

// Notify returns a channel that receives a signal when new rows are available.
func (r *RingBuffer) Notify() <-chan struct{} {
	return r.notify
}
