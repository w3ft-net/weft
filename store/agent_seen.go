package store

import (
	"context"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"strings"
	"sync"
	"time"
)

const (
	// seenRefresh is how stale a written last_seen_at may get before an
	// unchanged-version sighting is written again.
	seenRefresh = 5 * time.Minute
	// seenBatch bounds the rows per UPDATE statement.
	seenBatch = 500
)

type agentSeen struct {
	version string
	at      time.Time
}

// AgentSeenTracker batches agent_tokens.last_seen_* stamps in memory.
// IngestStream opens call Touch (a map write, no I/O); a flusher writes one
// statement per interval. Without it every stream open pays a database
// UPDATE, which ack cycling multiplies by hosts x files.
//
// The stamps are observability data (dashboard, deprecation window); a
// crash loses at most one interval of them and agents re-stamp on their
// next open. A nil *AgentSeenTracker ignores Touch.
type AgentSeenTracker struct {
	exec func(sql string, args ...any) error

	mu      sync.Mutex
	dirty   map[uint]agentSeen
	written map[uint]agentSeen // last value flushed, to suppress no-op writes
}

func NewAgentSeenTracker(s *Store) *AgentSeenTracker {
	return &AgentSeenTracker{
		exec: func(sql string, args ...any) error {
			return s.crossTenant().Exec(sql, args...).Error
		},
		dirty:   make(map[uint]agentSeen),
		written: make(map[uint]agentSeen),
	}
}

// Touch records that tokenID was seen at `at` running client `version`.
func (t *AgentSeenTracker) Touch(tokenID uint, version string, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.dirty[tokenID] = agentSeen{version, at}
	t.mu.Unlock()
}

// Flush writes pending stamps. Entries identical to what was last written
// (same version, within seenRefresh) are skipped. On error the unwritten
// stamps are put back without displacing anything newer.
//
// The WHERE guard means a stamp older than the row's current last_seen_at
// is a no-op, so two coordinators flushing their own views cannot regress
// each other.
func (t *AgentSeenTracker) Flush() error {
	t.mu.Lock()
	batch := t.dirty
	t.dirty = make(map[uint]agentSeen)
	for id, v := range batch {
		if w, ok := t.written[id]; ok && w.version == v.version && v.at.Sub(w.at) < seenRefresh {
			delete(batch, id)
		}
	}
	t.mu.Unlock()

	ids := make([]uint, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += seenBatch {
		chunk := ids[start:min(start+seenBatch, len(ids))]
		args := make([]any, 0, 3*len(chunk))
		vals := make([]string, 0, len(chunk))
		for _, id := range chunk {
			v := batch[id]
			vals = append(vals, "(CAST(? AS bigint), CAST(? AS text), CAST(? AS timestamptz))")
			args = append(args, id, v.version, v.at)
		}
		err := t.exec(`UPDATE agent_tokens t SET last_seen_version = v.version, last_seen_at = v.at
FROM (VALUES `+strings.Join(vals, ", ")+`) AS v(id, version, at)
WHERE t.id = v.id AND (t.last_seen_at IS NULL OR t.last_seen_at < v.at)`, args...)
		if err != nil {
			t.requeue(batch, ids[start:])
			return fmt.Errorf("flush agent last-seen: %w", err)
		}
		t.mu.Lock()
		for _, id := range chunk {
			t.written[id] = batch[id]
		}
		t.mu.Unlock()
	}
	return nil
}

func (t *AgentSeenTracker) requeue(batch map[uint]agentSeen, ids []uint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		if cur, ok := t.dirty[id]; !ok || cur.at.Before(batch[id].at) {
			t.dirty[id] = batch[id]
		}
	}
}

// Run flushes every interval (jittered) until ctx is done, then once more.
func (t *AgentSeenTracker) Run(ctx context.Context, interval time.Duration) {
	for {
		timer := time.NewTimer(defaultJitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			if err := t.Flush(); err != nil {
				weftsdk.Infof(ctx, "agent-seen: final flush: %v", err)
			}
			return
		case <-timer.C:
			if err := t.Flush(); err != nil {
				weftsdk.Infof(ctx, "agent-seen: %v", err)
			}
		}
	}
}
