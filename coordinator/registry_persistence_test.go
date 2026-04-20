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

// Rotation must persist the transition from INGESTING → READ_ONLY on
// the outgoing shard. Otherwise the store keeps claiming the shard is
// INGESTING long after rotation.
func TestRotate_PersistsReadOnlyTransition(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	// An INGESTING shard that will get rotated out because RowCount
	// exceeds MaxRows.
	old := &ShardEntry{
		ID:          "shard-old",
		Apps:        []string{"weft"},
		State:       StateIngesting,
		Shard:       &failingShard{id: "shard-old"},
		RowCount:    1_000_000_000,
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(100 * 365 * 24 * time.Hour),
		CreatedAt:   time.Now().Add(-time.Hour),
	}
	reg.Add(old)

	prov := &Provisioner{
		registry: reg,
		launcher: &stubLauncher{addr: "10.0.0.99:9000"},
		config: ProvisionerConfig{
			MaxRows:          100,
			PollInterval:     time.Second,
			WindowDuration:   time.Hour,
			ConfigureRetries: 1,
			ConfigureBackoff: time.Millisecond,
		},
		stats:      NewAppStats(),
		Buckets:    NewBucketManager(nil),
		configurer: &failingConfigurer{},
	}

	// rotate() is the code path that currently misses PersistState.
	prov.rotate(context.Background(), old)

	rec, ok := fake.get("shard-old")
	if !ok {
		t.Fatal("shard-old missing from store")
	}
	if rec.State != string(StateReadOnly) {
		t.Errorf("after rotate, store still says %q — PersistState missing", rec.State)
	}
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

// stubLauncher returns a canned address from LaunchInstance so the
// rotate() path can proceed past the launch step.
type stubLauncher struct {
	addr string
}

func (s *stubLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return s.addr, nil
}
func (s *stubLauncher) TerminateInstance(_ context.Context, _, _ string) error { return nil }

// failingConfigurer always errors on Dial. This lets us exercise the
// transition on the *outgoing* shard in rotate() without needing a real
// gRPC server for the replacement.
type failingConfigurer struct{}

func (failingConfigurer) Dial(_ context.Context, _ string, _ *pb.ConfigureRequest) (shard.Shard, error) {
	return nil, fmt.Errorf("configurer not wired in test")
}
