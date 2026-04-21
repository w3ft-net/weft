package coordinator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// fakeRegistryStore is an in-memory RegistryStore for tests that care
// about persistence behavior but don't want the overhead of real
// postgres.
type fakeRegistryStore struct {
	mu      sync.Mutex
	records map[string]store.ShardRecord
	saves   int
	deletes int
}

func newFakeRegistryStore() *fakeRegistryStore {
	return &fakeRegistryStore{records: make(map[string]store.ShardRecord)}
}

func (f *fakeRegistryStore) SaveShard(rec *store.ShardRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[rec.ID] = *rec
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

func (f *fakeRegistryStore) ListShards() ([]store.ShardRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.ShardRecord, 0, len(f.records))
	for _, r := range f.records {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRegistryStore) GetShard(id string) (store.ShardRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[id]
	if !ok {
		return store.ShardRecord{}, false, nil
	}
	return r, true, nil
}

func (f *fakeRegistryStore) get(id string) (store.ShardRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[id]
	return r, ok
}

// Rebind (instance replaced behind a durable shard id) must persist the
// new address so a coordinator restart reloads the correct endpoint.
func TestRebind_PersistsAddrChange(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	reg.Add(&ShardEntry{
		ID:         "shard-durable",
		State:      StateIngesting,
		Shard:      &failingShard{id: "shard-durable"},
		Addr:       "10.0.0.1:9000",
		InstanceID: "i-old",
		CreatedAt:  time.Now(),
	})

	// The rebind arm of adoptFromHeartbeats needs a heartbeat that
	// reports the same id at a different addr. Use a stub configurer
	// — it's not invoked on the rebind path, but it's required by the
	// provisioner struct.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-durable", Addr: "10.0.0.2:9000", InstanceID: "i-new", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry:   reg,
		config:     ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
		configurer: &stubConfigurer{},
	}

	prov.adoptFromHeartbeats()

	rec, _ := fake.get("shard-durable")
	if rec.Addr != "10.0.0.2:9000" {
		t.Errorf("store addr: got %q want 10.0.0.2:9000 — rebind forgot to persist", rec.Addr)
	}
	if rec.InstanceID != "i-new" {
		t.Errorf("store instance_id: got %q want i-new", rec.InstanceID)
	}
}

// PersistState must no-op when no store is configured (in-process tests
// pattern).
func TestPersistState_NoOpWhenStoreUnset(t *testing.T) {
	reg := NewShardRegistry()
	entry := &ShardEntry{ID: "x", State: StateIngesting, CreatedAt: time.Now()}
	reg.Add(entry)

	entry.State = StateReadOnly
	reg.PersistState(entry) // must not panic
}

// Sanity: Remove reaches the store when one is configured.
func TestRemove_DeletesFromStore(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	reg.Add(&ShardEntry{ID: "shard-bye", State: StateIngesting, CreatedAt: time.Now()})
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
