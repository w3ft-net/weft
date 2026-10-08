package store

import (
	"math/rand/v2"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// authCache is an in-process cache in front of ValidateAgentToken. Ack
// cycling (docs/plans/weftd-offset-checkpointing.md) makes IngestStream
// opens steady-state, and an uncached open costs three SELECTs (the token
// row plus its two Preloads). Keyed by the token hash, so the raw token is
// never held in memory.
//
// Revocation latency is bounded by the TTL on any process other than the
// one that performed the revoke (which invalidates immediately).
type authCache struct {
	mu      sync.RWMutex
	entries map[string]authEntry
	group   singleflight.Group

	ttl, negTTL time.Duration
	maxEntries  int

	now    func() time.Time
	jitter func(base time.Duration) time.Duration
}

type authEntry struct {
	result  *AgentAuthResult // nil = negative entry (no such token)
	expires time.Time
}

func newAuthCache(ttl, negTTL time.Duration, maxEntries int) *authCache {
	return &authCache{
		entries:    make(map[string]authEntry),
		ttl:        ttl,
		negTTL:     negTTL,
		maxEntries: maxEntries,
		now:        time.Now,
		jitter:     defaultJitter,
	}
}

// defaultJitter returns base ±10%, uniform. Entries created together (boot,
// a fleet reconnecting after a restart) would otherwise all expire in the
// same instant and stampede the database together, forever. This is load
// smoothing, not security, so math/rand is fine.
func defaultJitter(base time.Duration) time.Duration {
	spread := base / 10
	if spread <= 0 {
		return base
	}
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

// get returns the cached result for key, calling load on a miss or expiry.
// Concurrent misses for one key share a single load. Load errors are never
// cached. Callers receive a copy they may mutate.
func (c *authCache) get(key string, load func() (*AgentAuthResult, error)) (*AgentAuthResult, error) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if ok && c.now().Before(e.expires) {
		return cloneAuth(e.result), nil
	}

	v, err, _ := c.group.Do(key, func() (any, error) {
		r, err := load()
		if err != nil {
			return nil, err
		}
		c.put(key, r)
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	return cloneAuth(v.(*AgentAuthResult)), nil
}

func (c *authCache) put(key string, r *AgentAuthResult) {
	ttl := c.ttl
	if r == nil {
		ttl = c.negTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxEntries {
		c.evictLocked()
	}
	c.entries[key] = authEntry{result: cloneAuth(r), expires: c.now().Add(c.jitter(ttl))}
}

// evictLocked frees room: expired entries first, then arbitrary ones (map
// order) so a flood of distinct bad tokens cannot grow the map.
func (c *authCache) evictLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	for k := range c.entries {
		if len(c.entries) < c.maxEntries {
			return
		}
		delete(c.entries, k)
	}
}

// invalidateTokenID drops any positive entry for the given agent_tokens row.
func (c *authCache) invalidateTokenID(id uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.result != nil && e.result.TokenID == id {
			delete(c.entries, k)
		}
	}
}

func (c *authCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func cloneAuth(r *AgentAuthResult) *AgentAuthResult {
	if r == nil {
		return nil
	}
	out := *r
	if r.Organization != nil {
		org := *r.Organization
		out.Organization = &org
	}
	return &out
}
