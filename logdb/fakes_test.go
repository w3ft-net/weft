package logdb

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// memLeases is an exclusive, expiring coordination.Leases. The contract's own
// suite (coordtest) holds each real implementation to the same behavior; this
// one lets the election logic run without a database.
type memLeases struct {
	mu     sync.Mutex
	held   map[string]memLease
	failed error // returned by every call while set
}

type memLease struct {
	holder  string
	expires time.Time
}

func newMemLeases() *memLeases { return &memLeases{held: map[string]memLease{}} }

func (m *memLeases) AcquireLease(name, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed != nil {
		return false, m.failed
	}
	if cur, ok := m.held[name]; ok && cur.holder != holder && time.Now().Before(cur.expires) {
		return false, nil
	}
	m.held[name] = memLease{holder: holder, expires: time.Now().Add(ttl)}
	return true, nil
}

func (m *memLeases) ReleaseLease(name, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.held[name]; ok && cur.holder == holder {
		delete(m.held, name)
	}
	return nil
}

func (m *memLeases) HeldLeases(_ context.Context, holder string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n, l := range m.held {
		if l.holder == holder && time.Now().Before(l.expires) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// forget drops a lease as if it had expired, whoever held it.
func (m *memLeases) forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, name)
}

// memServices is a ServiceDirectory over a map: which services a bucket
// serves, with an unassigned one belonging to the default bucket.
type memServices struct {
	mu   sync.Mutex
	rows []memService
}

type memService struct {
	ns       uint
	service  string
	bucketID int64 // 0: unassigned
}

func (m *memServices) add(ns uint, bucketID int64, services ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range services {
		m.rows = append(m.rows, memService{ns: ns, service: s, bucketID: bucketID})
	}
}

func (m *memServices) ServicesInBucket(bucketID int64, unassigned bool) ([]shardstore.ShardService, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []shardstore.ShardService
	for _, r := range m.rows {
		if r.bucketID == bucketID || (unassigned && r.bucketID == 0) {
			out = append(out, shardstore.ShardService{NamespaceID: r.ns, Service: r.service})
		}
	}
	return out, nil
}

func (m *memServices) EnsureSource(ns uint, service string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ns == ns && r.service == service {
			return nil
		}
	}
	m.rows = append(m.rows, memService{ns: ns, service: service})
	return nil
}

// memStandbys is a StandbyStore over a slice, with the claim rules of the real
// one: a claim takes an unclaimed standby of the tier, AZ and image, preferring
// one that has heartbeat; a shard that already holds one gets it back.
type memStandbys struct {
	mu   sync.Mutex
	rows []Standby
}

func (m *memStandbys) ListStandbys() ([]Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Standby(nil), m.rows...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].LaunchedAt.Before(out[j].LaunchedAt) })
	return out, nil
}

func (m *memStandbys) InsertStandby(sp *Standby) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.InstanceID == sp.InstanceID {
			return nil
		}
	}
	m.rows = append(m.rows, *sp)
	return nil
}

func (m *memStandbys) DeleteStandby(instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rows {
		if r.InstanceID == instanceID {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			break
		}
	}
	return nil
}

func (m *memStandbys) StandbyClaimedBy(shardID string) (*Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimedBy(shardID), nil
}

func (m *memStandbys) claimedBy(shardID string) *Standby {
	var best *Standby
	for i := range m.rows {
		r := &m.rows[i]
		if r.ClaimedBy != nil && *r.ClaimedBy == shardID && (best == nil || r.ClaimedAt.Before(*best.ClaimedAt)) {
			best = r
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

func (m *memStandbys) ClaimStandby(shardID, tier, az, imageID string, at time.Time) (*Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held := m.claimedBy(shardID); held != nil {
		return held, nil
	}
	pick := -1
	for i, r := range m.rows {
		if r.ClaimedBy != nil || r.Tier != tier || r.AZ != az || r.ImageID != imageID {
			continue
		}
		switch {
		case pick < 0:
			pick = i
		case (r.LastHeartbeatAt != nil) != (m.rows[pick].LastHeartbeatAt != nil):
			if r.LastHeartbeatAt != nil {
				pick = i
			}
		case r.LaunchedAt.Before(m.rows[pick].LaunchedAt):
			pick = i
		}
	}
	if pick < 0 {
		return nil, nil
	}
	m.rows[pick].ClaimedBy, m.rows[pick].ClaimedAt = &shardID, &at
	cp := m.rows[pick]
	return &cp, nil
}

func (m *memStandbys) ObserveStandbyHeartbeat(instanceID, addr, binarySHA string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].InstanceID == instanceID {
			m.rows[i].Addr, m.rows[i].BinarySHA, m.rows[i].LastHeartbeatAt = addr, binarySHA, &at
		}
	}
	return nil
}

// standbyDB is a shard-state store with a warm pool beside it, the shape the
// hosted Postgres store has.
type standbyDB struct {
	shardstore.Store
	*memStandbys
}

func openStandbyTestStore(t *testing.T) standbyDB {
	t.Helper()
	return standbyDB{Store: openSMTestStore(t), memStandbys: &memStandbys{}}
}

// withoutBuckets is a store that no longer lists the buckets in gone, as if
// they were deleted: the contract has no bucket delete.
type withoutBuckets struct {
	shardstore.Store
	gone map[int64]bool
}

func (w withoutBuckets) ListBuckets() ([]shardstore.Bucket, error) {
	all, err := w.Store.ListBuckets()
	var out []shardstore.Bucket
	for _, b := range all {
		if !w.gone[b.ID] {
			out = append(out, b)
		}
	}
	return out, err
}

// stuckStore reports the given shards from StuckArchiving.
type stuckStore struct {
	shardstore.Store
	stuck []StuckShard
}

func (s stuckStore) StuckArchiving(before time.Time) ([]StuckShard, error) {
	var out []StuckShard
	for _, st := range s.stuck {
		if st.Since.Before(before) {
			out = append(out, st)
		}
	}
	return out, nil
}

// tierRetention is how far back a bucket of the given billing tier is queried.
// The tiers are the hosted build's; logdb only ever sees the earliest instant
// it was handed.
func tierRetention(tier string) time.Duration {
	switch tier {
	case "swatch":
		return 30 * 24 * time.Hour
	case "yard":
		return 90 * 24 * time.Hour
	case "bolt":
		return 180 * 24 * time.Hour
	}
	return 24 * time.Hour
}
