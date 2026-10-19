package service

import (
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ingestTokenBucket wraps a byte-rate limiter for one AgentToken. limit
// and burst are compared (not gen-counted) on every touch so a config
// change takes effect on the bucket in place — SetLimit/SetBurst, never
// replacing it, so an already-throttled token's accumulated wait isn't
// reset by an unrelated config edit.
type ingestTokenBucket struct {
	limiter            *rate.Limiter
	bytesPerSec, burst int64
	seen               int64 // unix-nano of last touch; atomic, used by the reaper
}

// ingestLimiter enforces a sustained bytes/sec budget per AgentToken —
// see docs/ingestion-syslog.md, "Rate limiting". Unlike IngestStreamBudgetBytes (a
// per-connection lifetime cap enforced for coordinator memory safety;
// see IngestStream), this bucket is keyed on the token and persists in
// memory across reconnects for the life of the process, so a customer
// can't dodge it by cycling connections. A row over budget is delayed
// until the bucket refills rather than dropped or disconnected — see
// wait.
type ingestLimiter struct {
	mu      sync.Mutex
	buckets map[uint]*ingestTokenBucket
}

func newIngestLimiter() *ingestLimiter {
	l := &ingestLimiter{buckets: make(map[uint]*ingestTokenBucket)}
	go l.reap()
	return l
}

// reap walks the bucket map every 5 min and drops tokens that haven't
// ingested in 30 min, capping memory growth under token churn. Mirrors
// api/ratelimit.go's limiter.reap.
func (l *ingestLimiter) reap() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-30 * time.Minute).UnixNano()
		l.mu.Lock()
		for k, b := range l.buckets {
			if atomic.LoadInt64(&b.seen) < cutoff {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

func (l *ingestLimiter) bucketFor(tokenID uint, bytesPerSec, burst int64) *ingestTokenBucket {
	l.mu.Lock()
	b, ok := l.buckets[tokenID]
	if !ok {
		b = &ingestTokenBucket{
			limiter:     rate.NewLimiter(rate.Limit(bytesPerSec), int(burst)),
			bytesPerSec: bytesPerSec,
			burst:       burst,
		}
		l.buckets[tokenID] = b
	} else if b.bytesPerSec != bytesPerSec || b.burst != burst {
		b.limiter.SetLimit(rate.Limit(bytesPerSec))
		b.limiter.SetBurst(int(burst))
		b.bytesPerSec, b.burst = bytesPerSec, burst
	}
	l.mu.Unlock()
	atomic.StoreInt64(&b.seen, time.Now().UnixNano())
	return b
}

// wait blocks until tokenID's bucket has room for n bytes, throttling a
// stream that's running hot rather than closing it — a customer well
// over budget slows to the configured rate instead of a thrash of
// reject/reconnect cycles (the bucket doesn't reset on reconnect, so
// reconnecting wouldn't help them anyway). bytesPerSec or burst <= 0
// disables the check entirely. A single row larger than burst is let
// through immediately (logged once) rather than blocked forever —
// LogRows are capped by GRPCMaxRecvBytes and are never expected to
// approach a sanely configured burst.
func (l *ingestLimiter) wait(ctx context.Context, tokenID uint, n int64, bytesPerSec, burst int64) error {
	if bytesPerSec <= 0 || burst <= 0 || n <= 0 {
		return nil
	}
	b := l.bucketFor(tokenID, bytesPerSec, burst)
	res := b.limiter.ReserveN(time.Now(), int(n))
	if !res.OK() {
		weftsdk.Infof(ctx, "ingest-ratelimit: token=%d row of %d bytes exceeds burst of %d bytes, allowing through unthrottled", tokenID, n, burst)
		return nil
	}
	delay := res.Delay()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		res.Cancel()
		return ctx.Err()
	}
}
