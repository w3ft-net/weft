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
	}

	prov.adoptFromHeartbeats()

	if got := len(configurer.calls); got != 0 {
		t.Fatalf("expected 0 Configure calls for known id, got %d", got)
	}
	if reg.Count() != 1 {
		t.Errorf("expected 1 registry entry, got %d", reg.Count())
	}
}
