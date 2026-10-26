package coordinator

import (
	"context"
	"fmt"
	"sync"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// Each shard gets its own send path: a FIFO queue and a goroutine that
// writes its batches in order. A shard whose Ingest is slow or held — one
// paused mid-upgrade holds the call until its successor opens (docs/plans/
// shard-seamless-upgrade.md) — then stalls only its own rows, never every
// write on this coordinator.
const (
	// maxQueuedRowsPerShard bounds one shard's backlog. It sits well above a
	// pause's worth of rows at peak (the hold is bounded by the shard
	// client's pauseHoldTimeout) and well below the ring buffer, so a shard
	// that is truly stuck can't consume the buffer the others need.
	maxQueuedRowsPerShard = 200_000
	// senderIdleTimeout retires a shard's sender after it has had nothing
	// to send for this long, so rotated-out shards don't keep a goroutine.
	senderIdleTimeout = time.Minute
)

// drainBatch is one routed group of rows bound for one shard.
type drainBatch struct {
	key  drainKey
	nsID uint
	sh   shard.Shard
	rows []*pb.LogRow
}

// shardSender is one shard's queue. Its goroutine runs while the queue is
// non-empty or has been empty for less than senderIdleTimeout.
type shardSender struct {
	mu     sync.Mutex
	queue  []drainBatch
	rows   int
	wake   chan struct{}
	exited bool // the goroutine has retired; a new batch needs a new sender
}

// StartDrain runs a background goroutine that drains the ring buffer
// and routes each row by (customer, namespace, service) to an INGESTING
// shard, handing the batch to that shard's sender.
func (c *Coordinator) StartDrain(ctx context.Context) {
	if c.Ingest == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.Ingest.Notify():
			}

			for {
				batch := c.Ingest.Drain(500)
				if batch == nil {
					break
				}

				// Group by (customer, namespace, service) for targeted
				// routing; count bytes per customer. The customer must be
				// part of the key — two customers can use the same
				// (namespace, service), but they resolve to different
				// namespace_ids and so route to different shards.
				byKey := make(map[drainKey][]*pb.LogRow)
				var order []drainKey
				for _, row := range batch {
					k := drainKey{row.CustomerId, row.Namespace, row.Service}
					if _, seen := byKey[k]; !seen {
						order = append(order, k)
					}
					byKey[k] = append(byKey[k], row)
					if c.Usage != nil && row.CustomerId != 0 {
						c.Usage.Record(row.CustomerId, int64(len(row.Raw)))
					}
				}

				for _, k := range order {
					rows := byKey[k]
					// Route by (namespace_id, service). namespace_id is
					// resolved through a (customer, namespace) cache so the
					// per-batch grouping keeps the DB off the hot path.
					nsID := c.ingestNamespaceID(k.customerID, k.namespace)
					// Strict service-scoped routing only — no whole-registry
					// fallback, which would commingle a different tenant's
					// rows onto the wrong shard. A service onboarded after
					// the INGESTING shards christened is stamped onto them by
					// the reconciler (OnboardService triggers it on first-seen);
					// rows that arrive before that lands go to DLQ and the
					// scanner re-ingests once a shard serves them.
					sh := c.Provisioner.IngestingShard(nsID, k.service)
					if sh == nil {
						dlqPath := c.DLQ.Write(rows)
						c.Errors.Record("none", "no shard serves "+k.namespace+"/"+k.service, rows, dlqPath)
						weftsdk.Infof(ctx, "drain: DLQ'd %d rows for %s (no shard serves this service)", len(rows), k.namespace+"/"+k.service)
						continue
					}
					c.enqueue(ctx, drainBatch{key: k, nsID: nsID, sh: sh, rows: rows})
				}
			}
		}
	}()
}

// enqueue appends b to its shard's queue, starting the shard's sender if
// it has none. A backlog past maxQueuedRowsPerShard goes to the DLQ.
func (c *Coordinator) enqueue(ctx context.Context, b drainBatch) {
	id := b.sh.ID()
	for {
		c.sendersMu.Lock()
		if c.senders == nil {
			c.senders = make(map[string]*shardSender)
		}
		s, ok := c.senders[id]
		if !ok {
			s = &shardSender{wake: make(chan struct{}, 1)}
			c.senders[id] = s
			go c.runSender(ctx, id, s)
		}
		c.sendersMu.Unlock()

		s.mu.Lock()
		if s.exited {
			// Retired between the map read and now; it removed itself from
			// the map, so the next pass starts a fresh one.
			s.mu.Unlock()
			continue
		}
		if s.rows+len(b.rows) > maxQueuedRowsPerShard {
			backlog := s.rows
			s.mu.Unlock()
			dlqPath := c.DLQ.Write(b.rows)
			err := fmt.Errorf("shard %s send queue full (%d rows waiting)", id, backlog)
			c.Errors.Record(id, err.Error(), b.rows, dlqPath)
			weftsdk.Errorf(ctx, "drain: DLQ'd %d rows for %s: %v", len(b.rows), b.key.namespace+"/"+b.key.service, err)
			return
		}
		s.queue = append(s.queue, b)
		s.rows += len(b.rows)
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
		return
	}
}

// runSender writes one shard's batches in order until it has been idle
// for senderIdleTimeout or ctx ends.
func (c *Coordinator) runSender(ctx context.Context, id string, s *shardSender) {
	idle := time.NewTimer(senderIdleTimeout)
	defer idle.Stop()
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				if !idle.Stop() {
					<-idle.C
				}
				idle.Reset(senderIdleTimeout)
				continue
			case <-idle.C:
				// Retire under both locks so enqueue either sees us in the
				// map with exited unset (and its batch is ours to send) or
				// sees exited and starts a fresh sender.
				c.sendersMu.Lock()
				s.mu.Lock()
				if len(s.queue) > 0 {
					s.mu.Unlock()
					c.sendersMu.Unlock()
					idle.Reset(senderIdleTimeout)
					continue
				}
				s.exited = true
				delete(c.senders, id)
				s.mu.Unlock()
				c.sendersMu.Unlock()
				return
			}
		}
		b := s.queue[0]
		s.queue[0] = drainBatch{}
		s.queue = s.queue[1:]
		s.rows -= len(b.rows)
		s.mu.Unlock()
		c.ingestBatch(ctx, b)
	}
}

// ingestBatch writes one batch to its shard, retrying once on another
// INGESTING candidate before giving the rows to the DLQ.
func (c *Coordinator) ingestBatch(ctx context.Context, b drainBatch) {
	app := b.key.namespace + "/" + b.key.service
	// sh.Ingest reports its own outcome to the provisioner — incrementing
	// the consecutive-failure counter on error, resetting it on success.
	// The returned error names the shard and the running failure count
	// (N/M) for the log line. (Routing exclusion is the FSM's job now, not
	// a per-failure ingest latch.)
	err := b.sh.Ingest(ctx, b.rows, c.Provisioner)
	if err == nil {
		c.Provisioner.Stats().Record(app, int64(len(b.rows)))
		return
	}
	weftsdk.Infof(ctx, "drain: %v", err)

	// Retry once, on a shard other than the one that just failed: it stays a
	// candidate until it has failed several times in a row, and the round-robin
	// counter every sender advances would send the retry straight back to it.
	if fallback := c.Provisioner.IngestingShardExcept(b.nsID, b.key.service, b.sh.ID()); fallback != nil {
		if retryErr := fallback.Ingest(ctx, b.rows, c.Provisioner); retryErr == nil {
			c.Provisioner.Stats().Record(app, int64(len(b.rows)))
			return
		}
	}

	// All retries exhausted — DLQ.
	dlqPath := c.DLQ.Write(b.rows)
	c.Errors.Record(b.sh.ID(), err.Error(), b.rows, dlqPath)
	weftsdk.Errorf(ctx, "drain: DLQ'd %d rows for %s (shard %s failed, no fallback)", len(b.rows), app, b.sh.ID())
}

// StartDLQReplay feeds the DLQ back into the ingest path every interval
// while IsLeader reports true. Rows that still can't be delivered are
// DLQ'd again under a new key, so a replay that finds every shard down
// costs one S3 round trip, not data.
func (c *Coordinator) StartDLQReplay(ctx context.Context, interval time.Duration, IsLeader func() bool) {
	if c.DLQ == nil {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !IsLeader() {
				continue
			}
			n, err := c.DLQ.Replay(ctx, c.Ingest.Write)
			if n > 0 {
				weftsdk.Infof(ctx, "dlq: replayed %d rows into the ingest path", n)
			}
			if err != nil {
				weftsdk.Errorf(ctx, "dlq: replay: %v", err)
			}
		}
	}()
}
