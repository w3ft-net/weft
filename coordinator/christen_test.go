package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// stubConfigurer records Configure calls and returns a fixed Shard handle
// for each. Replaces the real gRPC dial in tests.
type stubConfigurer struct {
	mu    sync.Mutex
	calls []struct {
		Addr string
		Req  *pb.ConfigureRequest
	}
	shardByID map[string]shard.Shard
}

func (s *stubConfigurer) Dial(_ context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, struct {
		Addr string
		Req  *pb.ConfigureRequest
	}{Addr: addr, Req: req})
	if s.shardByID == nil {
		s.shardByID = make(map[string]shard.Shard)
	}
	ds, err := shard.NewDuckDBShard(req.ShardId, "")
	if err != nil {
		return nil, err
	}
	s.shardByID[req.ShardId] = ds
	return ds, nil
}

// An unclaimed heartbeat (empty shard_id) should trigger christening:
// the provisioner mints a shard id, sends Configure with a non-zero
// window, and registers the result as INGESTING. This is the code path
// that doesn't exist yet — `adoptFromHeartbeats` currently slots empty-
// id rows into the registry with zero-valued windows.
func TestChristen_UnclaimedHeartbeat_GetsWindowAndID(t *testing.T) {
	reg := NewShardRegistry()
	configurer := &stubConfigurer{}

	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "", Addr: "10.0.0.50:9000", State: "ADOPTED", InstanceID: "i-abc"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			Store:          mockStore,
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
		},
		configurer: configurer,
		Buckets:    newDefaultStubBuckets(),
	}

	prov.adoptFromHeartbeats()

	// Exactly one Configure call issued, at the heartbeat's address.
	if got := len(configurer.calls); got != 1 {
		t.Fatalf("expected 1 Configure call, got %d", got)
	}
	call := configurer.calls[0]
	if call.Addr != "10.0.0.50:9000" {
		t.Errorf("expected Configure at 10.0.0.50:9000, got %s", call.Addr)
	}

	// Shard id must be non-empty (minted by the coordinator).
	if call.Req.ShardId == "" {
		t.Error("expected non-empty minted ShardId")
	}

	// Window must be 7d wide per WindowDuration config.
	start, err1 := time.Parse(time.RFC3339, call.Req.WindowStart)
	end, err2 := time.Parse(time.RFC3339, call.Req.WindowEnd)
	if err1 != nil || err2 != nil {
		t.Fatalf("window times not RFC3339: %v / %v", err1, err2)
	}
	if got := end.Sub(start); got != 7*24*time.Hour {
		t.Errorf("window duration: got %v want 168h", got)
	}

	// Registered and INGESTING.
	entry := reg.Get(call.Req.ShardId)
	if entry == nil {
		t.Fatalf("minted shard %q not registered", call.Req.ShardId)
	}
	if entry.State != StateIngesting {
		t.Errorf("state: got %s want INGESTING", entry.State)
	}
	if entry.InstanceID != "i-abc" {
		t.Errorf("InstanceID: got %q want i-abc", entry.InstanceID)
	}
	if entry.WindowStart.IsZero() || entry.WindowEnd.IsZero() {
		t.Error("expected non-zero windows on christened entry")
	}
}

// ProcessHeartbeat lets the Heartbeat RPC drive christen/rebind/adopt
// inline rather than wait up to PollInterval for the next tick. A
// single unclaimed heartbeat should mint + Configure one shard.
func TestProcessHeartbeat_ChristensInlineForUnclaimed(t *testing.T) {
	reg := NewShardRegistry()
	configurer := &stubConfigurer{}

	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
		},
		configurer: configurer,
		Buckets:    newDefaultStubBuckets(),
	}

	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    "",
		Addr:       "10.0.0.50:9000",
		InstanceID: "i-new",
		State:      "ADOPTED",
	})

	if len(configurer.calls) != 1 {
		t.Fatalf("expected 1 Configure call, got %d", len(configurer.calls))
	}
	if configurer.calls[0].Addr != "10.0.0.50:9000" {
		t.Errorf("Configure addr: got %q", configurer.calls[0].Addr)
	}
	if reg.Count() != 1 {
		t.Errorf("expected 1 registered shard, got %d", reg.Count())
	}
}

// TestChristen_ConcurrentHeartbeats_FromSameAddr_DoNotDoubleConfigure
// exercises the racy-christen bug we saw in prod (2026-04-20 coord log):
//
//   provisioner: christened shard-YYYYMMDDT0000-2 at 172.31.128.108:9000
//   christen 172.31.128.108:9000: configure shard-YYYYMMDDT0000-3 at
//   172.31.128.108:9000: rpc error: code = Unknown desc = shard already
//   configured as shard-YYYYMMDDT0000-2
//
// Two heartbeats from the same unclaimed instance arrive close together.
// The Heartbeat RPC spawns a goroutine per call, so ProcessHeartbeat
// runs twice concurrently. heartbeatMu serialises them, but the second
// goroutine's captured hb.ShardID is still "" (the shard only gets an
// id after Configure returns) — so it re-enters christen() and tries
// to Configure the same addr a second time with a new id.
//
// Expected behaviour: exactly one Configure call per unclaimed addr,
// regardless of how many concurrent empty-id heartbeats arrive.
// Today: two Configure calls.
func TestChristen_ConcurrentHeartbeats_FromSameAddr_DoNotDoubleConfigure(t *testing.T) {
	reg := NewShardRegistry()
	configurer := &stubConfigurer{}

	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
		},
		configurer: configurer,
		Buckets:    newDefaultStubBuckets(),
	}

	hb := store.ShardHeartbeat{
		ShardID:    "",
		Addr:       "172.31.128.108:9000",
		InstanceID: "i-racy",
		State:      "ADOPTED",
	}

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			prov.ProcessHeartbeat(hb)
		}()
	}
	wg.Wait()

	configurer.mu.Lock()
	calls := len(configurer.calls)
	configurer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected 1 Configure for %d concurrent heartbeats at same addr, got %d", N, calls)
	}
	if reg.Count() != 1 {
		t.Errorf("expected 1 registry entry, got %d", reg.Count())
	}
}

// TestAdopt_DoesNotClobberPersistedWindowAndToken exercises the adopt
// bug we saw in prod: a freshly-rebooted coordinator doesn't load live
// shards from the DB on startup (cmd/coordinator/main.go filters
// LoadFromStore to ARCHIVED only). The live shard's next heartbeat
// lands on the adopt branch in ProcessHeartbeat, which fabricates a
// new ShardEntry WITHOUT WindowStart/WindowEnd/HeartbeatToken and
// calls registry.Add → persist → SaveShard. SaveShard is an UPSERT,
// so the zero-valued fields overwrite the correct values already
// sitting in the DB.
//
// Repro in the test: pre-populate a fakeRegistryStore with a record
// that has a populated window + heartbeat token (as a real DB would
// after a prior christen). Do NOT add the entry to the in-memory
// registry — simulating the gap between LoadFromStore and the first
// heartbeat. Drive a heartbeat with the matching ShardID. The adopt
// path fires, persists, and (today) blanks the DB fields.
//
// Expected: window + token still populated in the fake store.
// Today: blanked.
func TestAdopt_DoesNotClobberPersistedWindowAndToken(t *testing.T) {
	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	// Pre-existing DB state from a prior christen — the sort of row
	// that LoadFromStore currently skips because State != ARCHIVED.
	originalWindow := "2026-04-20T00:00:00Z"
	originalEnd := "2026-04-27T00:00:00Z"
	originalToken := "cf5c60de2f11b5af0a97b8dee2ca7c0705a00bef21c95e35f6cef2ba68bd5cec"
	fake.records["shard-20260420T0000-1"] = store.ShardRecord{
		ID:             "shard-20260420T0000-1",
		Apps:           "null",
		WindowStart:    originalWindow,
		WindowEnd:      originalEnd,
		State:          string(StateIngesting),
		Addr:           "10.0.0.50:9000",
		InstanceID:     "i-old",
		CreatedAt:      "2026-04-20T00:30:53Z",
		HeartbeatToken: originalToken,
	}

	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
			ShardSecret:    "test-secret",
		},
		configurer: &stubConfigurer{},
		Buckets:    newDefaultStubBuckets(),
	}

	// Shard heartbeats with its durable id. Registry has no entry
	// (LoadFromStore filtered it out), so ProcessHeartbeat takes the
	// adopt branch.
	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    "shard-20260420T0000-1",
		Addr:       "10.0.0.50:9000",
		InstanceID: "i-old",
		State:      "INGESTING",
	})

	got, ok := fake.get("shard-20260420T0000-1")
	if !ok {
		t.Fatal("record should still exist after adopt")
	}
	if got.WindowStart != originalWindow {
		t.Errorf("WindowStart clobbered: got %q, want %q", got.WindowStart, originalWindow)
	}
	if got.WindowEnd != originalEnd {
		t.Errorf("WindowEnd clobbered: got %q, want %q", got.WindowEnd, originalEnd)
	}
	if got.HeartbeatToken != originalToken {
		t.Errorf("HeartbeatToken clobbered: got %q, want %q (zero/empty token = adopt path overwrote)",
			got.HeartbeatToken, originalToken)
	}
}

// A heartbeat from a known shard id must NOT trigger christening — that
// would re-configure an already-identified shard. It should re-bind
// addr/instance on the existing entry instead.
func TestChristen_KnownIDTakesRebindPath(t *testing.T) {
	reg := NewShardRegistry()
	existing := &failingShard{id: "shard-existing"}
	reg.Add(&ShardEntry{
		ID:    "shard-existing",
		State: StateIngesting,
		Shard: existing,
		Addr:  "10.0.0.1:9000",
	})

	configurer := &stubConfigurer{}
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			// Same id, same addr — just a liveness ping. No Configure.
			{ShardID: "shard-existing", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			Store:          mockStore,
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
		},
		configurer: configurer,
		Buckets:    newDefaultStubBuckets(),
	}

	prov.adoptFromHeartbeats()

	if got := len(configurer.calls); got != 0 {
		t.Fatalf("expected 0 Configure calls for known id, got %d", got)
	}
	if reg.Count() != 1 {
		t.Errorf("expected 1 registry entry, got %d", reg.Count())
	}
}
