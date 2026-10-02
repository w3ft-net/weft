package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// SchemaCache is a Valkey-backed cache-aside layer in front of
// log_groups.schema_yaml, shared across every api+coordinator node —
// see docs/plans/cloudwatch-kinesis-subscription.md's ingest-enrichment
// discussion. Ingest-time schema lookups (api.enrichRow/enrichRows,
// SyslogServer) happen per log line; without a shared cache, each node
// would hit Postgres on every line for services it hasn't seen recently,
// and a syslog connection's per-frame lookups have no request boundary
// to locally memoize across the way a batched HTTP POST does.
//
// Same best-effort philosophy as coordinator's redisFanout
// (tail_fanout_redis.go): no Ping at construction (a transient
// ElastiCache blip must not block ingest startup), every operation is
// wrapped in a short timeout and swallows its own error — a struggling
// or unreachable cache degrades to "every lookup falls through to
// Postgres" (FindLogGroupSchema's caller-side behavior), never a
// dropped or delayed log line. SchemaCache is nil-safe throughout so a
// *Store constructed without one (tests, weft-admin, weft-migrate,
// weft-demo-seed) just always falls through.
type SchemaCache struct {
	client *redis.Client
}

// NewSchemaCache connects to Valkey/Redis at addr. Mirrors
// coordinator.newRedisFanout's connection setup exactly (same TLS
// posture, same "don't verify connectivity yet" stance) since both
// point at the same shared ElastiCache instance.
func NewSchemaCache(addr string, tlsEnabled bool) *SchemaCache {
	opts := &redis.Options{Addr: addr}
	if tlsEnabled {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &SchemaCache{client: redis.NewClient(opts)}
}

// schemaCacheOpTimeout bounds every individual cache op. Ingest sits in
// the request/frame hot path, so a stalled Redis connection must fail
// fast into the Postgres fallback rather than stall a log line by
// go-redis's multi-second default timeouts.
const schemaCacheOpTimeout = 200 * time.Millisecond

// schemaCachePositiveTTL/NegativeTTL are a safety net for a missed
// invalidation (e.g. a write landing during a Valkey blip, or a direct-
// DB caller like weft-demo-seed that isn't wired to a cache at all) —
// not the primary correctness mechanism. That's AddLogGroup/
// ConfigureLogGroup's active DEL on every successful write. Negative
// entries (service not configured yet) get a much shorter TTL: an
// unconfigured service is actively being onboarded, and a 5-minute
// stale "not configured" would delay a customer seeing their first
// parsed rows right after finishing setup.
const (
	schemaCachePositiveTTL = 5 * time.Minute
	schemaCacheNegativeTTL = 30 * time.Second
)

// cachedSchemaEntry is the cache's wire format. Storing the raw
// schema_yaml string rather than a compiled *ingest.Parser: re-parsing
// YAML (and recompiling LevelPattern's regex, if set) per cache hit is
// microseconds — negligible next to the network round trip this cache
// exists to avoid — and a *ingest.Parser isn't cleanly serializable
// (it holds a compiled *regexp.Regexp).
type cachedSchemaEntry struct {
	Configured bool   `json:"configured"`
	SchemaYAML string `json:"schema_yaml,omitempty"`
}

func schemaCacheKey(orgID OrgID, namespace string, service string) string {
	return "schema:" + orgID.String() + ":" + namespace + ":" + service
}

// get returns (entry, true) on a cache hit, (nil, false) on a miss OR
// any cache-layer error — callers can't distinguish the two and
// shouldn't need to: either way, fall through to Postgres.
func (c *SchemaCache) get(orgID OrgID, namespace string, service string) (*cachedSchemaEntry, bool) {
	if c == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), schemaCacheOpTimeout)
	defer cancel()
	val, err := c.client.Get(ctx, schemaCacheKey(orgID, namespace, service)).Result()
	if err != nil {
		return nil, false // includes redis.Nil (real miss) and any transport error
	}
	var entry cachedSchemaEntry
	if err := json.Unmarshal([]byte(val), &entry); err != nil {
		return nil, false
	}
	return &entry, true
}

// set populates the cache after a Postgres lookup (cache-aside fill).
// Best-effort: a write failure just means the next lookup falls through
// to Postgres again, same as a cold cache.
func (c *SchemaCache) set(orgID OrgID, namespace string, service string, entry *cachedSchemaEntry) {
	if c == nil {
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	ttl := schemaCachePositiveTTL
	if !entry.Configured {
		ttl = schemaCacheNegativeTTL
	}
	ctx, cancel := context.WithTimeout(context.Background(), schemaCacheOpTimeout)
	defer cancel()
	if err := c.client.Set(ctx, schemaCacheKey(orgID, namespace, service), b, ttl).Err(); err != nil {
		log.Printf("schema cache: set %s/%s: %v", namespace, service, err)
	}
}

// invalidate is called by AddLogGroup/ConfigureLogGroup after a
// successful write. A plain DEL is sufficient correctness-wise — every
// node reads this cache fresh on each ingest-time lookup (there is no
// longer-lived local tier above it to also evict), so removing the key
// here makes every node's very next lookup for this service miss and
// re-fetch the new schema from Postgres. No pub/sub broadcast needed
// unless a local per-process tier is added later, at which point this
// is the natural place to also PUBLISH an eviction notice.
func (c *SchemaCache) invalidate(orgID OrgID, namespace string, service string) {
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), schemaCacheOpTimeout)
	defer cancel()
	if err := c.client.Del(ctx, schemaCacheKey(orgID, namespace, service)).Err(); err != nil {
		log.Printf("schema cache: invalidate %s/%s: %v", namespace, service, err)
	}
}
