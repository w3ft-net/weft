package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// fakeRegistryStore is an in-memory RegistryStore for tests that care
// about persistence behavior but don't want the overhead of real
// postgres.
type fakeRegistryStore struct {
	mu      sync.Mutex
	records map[string]*Shard
	saves   int
	deletes int
	// servingFn, when set, backs ShardIDsServingService so a test can
	// drive ShardDescriptorsFor's serving-set filter. Nil falls back to
	// "every known shard serves it" — the fake doesn't model
	// shard_services, so the permissive default keeps routing tests that
	// don't care about the stamp from silently filtering to zero.
	servingFn func(namespaceID uint, service string) (map[string]struct{}, error)
	// shardServices accumulates rows written via AddShardService /
	// StampShardServices; ListShardServices returns them so the registry's
	// load/refresh paths see a shard's membership. bucketServices, when
	// set per bucketID, backs ServicesForBucket (and what StampShardServices
	// stamps); christen/reconciler tests set it to model a bucket's
	// log_groups.
	shardServices  []store.ShardService
	fieldFacts     []store.ShardFieldFact
	bucketServices map[int64][]store.ShardService
	// ingestFailureWrites counts SetShardIngestFailures calls — separate
	// from saves (SaveShard) since RecordIngestFailure/ResetIngestFailures
	// persist via the narrow write, not a full SaveShard.
	ingestFailureWrites int
	// touches counts TouchHeartbeat calls.
	touches int
}

func newFakeRegistryStore() *fakeRegistryStore {
	return &fakeRegistryStore{records: make(map[string]*Shard)}
}

// SaveShard stores a persistedCopy so the fake's record is isolated from
// the caller's live entry (modelling the DB round-trip), matching how the
// real ShardRegistry hands the store a snapshot.
func (f *fakeRegistryStore) SaveShard(sh *Shard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[sh.ID] = sh.persistedCopy()
	f.saves++
	return nil
}

func (f *fakeRegistryStore) DeleteShard(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, id)
	f.deletes++
	return nil
}

// ListShards returns fresh copies so the registry's load path gets new
// objects rather than aliasing the fake's stored records (as gorm would).
func (f *fakeRegistryStore) ListShards() ([]*Shard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Shard, 0, len(f.records))
	for _, r := range f.records {
		out = append(out, r.persistedCopy())
	}
	return out, nil
}

func (f *fakeRegistryStore) GetShard(id string) (*Shard, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[id]
	if !ok {
		return nil, false, nil
	}
	return r.persistedCopy(), true, nil
}

func (f *fakeRegistryStore) TouchHeartbeat(id string, rowCount int64, at time.Time, binarySHA string, diskBytes int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches++
	if rec, ok := f.records[id]; ok {
		rec.RowCount = rowCount
		rec.DiskBytes = diskBytes
		rec.LastHeartbeatAt = at
		rec.BinarySHA = binarySHA
	}
	return nil
}

func (f *fakeRegistryStore) SetShardIngestFailures(id string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingestFailureWrites++
	if rec, ok := f.records[id]; ok {
		rec.IngestFailures = n
	}
	return nil
}

// get returns the stored record for test inspection.
func (f *fakeRegistryStore) get(id string) (*Shard, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[id]
	return r, ok
}

// ── ShardOpsStore ────────────────────────────────────────

// StampShardServices stamps the bucket's services (from bucketServices)
// onto shardID, modelling the real store's log_groups → shard_services
// copy so christen/load tests see the right membership.
func (f *fakeRegistryStore) StampShardServices(shardID string, bucketID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.bucketServices[bucketID] {
		f.shardServices = append(f.shardServices, store.ShardService{
			ShardID: shardID, NamespaceID: s.NamespaceID, Service: s.Service,
		})
	}
	return nil
}

// ListShardServices returns every stamped/added row.
func (f *fakeRegistryStore) ListShardServices() ([]store.ShardService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ShardService(nil), f.shardServices...), nil
}

// ListShardFieldFacts returns every typed fact applied so far.
func (f *fakeRegistryStore) ListShardFieldFacts() ([]store.ShardFieldFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ShardFieldFact(nil), f.fieldFacts...), nil
}

// ApplyShardFieldFacts upserts typed rows and deletes dropped ones.
func (f *fakeRegistryStore) ApplyShardFieldFacts(shardID string, typed, dropped []store.ShardFieldFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	same := func(a, b store.ShardFieldFact) bool {
		a.Epoch, b.Epoch, a.ShardID, b.ShardID = 0, 0, "", ""
		return a == b
	}
	var kept []store.ShardFieldFact
	for _, r := range f.fieldFacts {
		drop := false
		for _, d := range append(append([]store.ShardFieldFact(nil), dropped...), typed...) {
			if r.ShardID == shardID && same(r, d) {
				drop = true
			}
		}
		if !drop {
			kept = append(kept, r)
		}
	}
	for _, t := range typed {
		t.ShardID = shardID
		kept = append(kept, t)
	}
	f.fieldFacts = kept
	return nil
}

// AddShardService appends a single (shard, namespace, service) row.
func (f *fakeRegistryStore) AddShardService(shardID string, namespaceID uint, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shardServices = append(f.shardServices, store.ShardService{
		ShardID: shardID, NamespaceID: namespaceID, Service: service,
	})
	return nil
}

// ServicesForBucket returns the bucket's modelled services (the
// log_groups stand-in tests set via bucketServices).
func (f *fakeRegistryStore) ServicesForBucket(bucketID int64) ([]store.ShardService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ShardService(nil), f.bucketServices[bucketID]...), nil
}

// ShardIDsServingService returns the serving set for read routing. With
// no servingFn override it returns every known shard ID — see the field
// comment for why the fake defaults permissive.
func (f *fakeRegistryStore) ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error) {
	if f.servingFn != nil {
		return f.servingFn(namespaceID, service)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.records))
	for id := range f.records {
		out[id] = struct{}{}
	}
	return out, nil
}

// PersistState must no-op when no store is configured (in-process tests
// pattern).
func TestPersistState_NoOpWhenStoreUnset(t *testing.T) {
	reg := NewShardRegistry()
	entry := &Shard{ID: "x", State: StateIngesting, CreatedAt: time.Now()}
	reg.Add(entry)

	entry.State = StateReadOnly
	reg.PersistState(entry) // must not panic
}

// Sanity: Remove reaches the store when one is configured.
func TestRemove_DeletesFromStore(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	reg.Add(&Shard{ID: "shard-bye", State: StateIngesting, CreatedAt: time.Now()})
	if _, ok := fake.get("shard-bye"); !ok {
		t.Fatal("shard-bye missing after Add")
	}

	reg.Remove("shard-bye")
	if _, ok := fake.get("shard-bye"); ok {
		t.Fatal("shard-bye still in store after Remove")
	}
	if fake.deletes != 1 {
		t.Errorf("expected 1 DeleteShard call, got %d", fake.deletes)
	}
}

// stubLauncher returns a canned address from LaunchInstance so tests
// that exercise check() can proceed past the launch step without a
// real gRPC server.
type stubLauncher struct {
	addr string
}

func (s *stubLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return s.addr, nil
}
func (s *stubLauncher) TerminateInstance(_ context.Context, _, _ string) error { return nil }

// failingConfigurer always errors on Dial. Keeps check()-path tests
// focused on registry transitions without needing a live replacement.
type failingConfigurer struct{}

func (failingConfigurer) Dial(_ context.Context, _ string, _ *pb.ConfigureRequest) (shard.Shard, error) {
	return nil, fmt.Errorf("configurer not wired in test")
}
