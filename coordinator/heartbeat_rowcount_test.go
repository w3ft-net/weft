package coordinator

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestProcessHeartbeat_UpdatesRowCount_SameAddr pins the steady-state
// hot path: every ~10s the shard heartbeats from the same address with
// its current row count. The handler must lift hb.RowCount onto the
// registry entry; without it dashboards perpetually show 0 even after
// gigabytes of ingest. Surfaced 2026-05-03 when /opt/app/data/shard.duckdb
// was 6.5 MiB on disk while shard_records.row_count read 0.
func TestProcessHeartbeat_UpdatesRowCount_SameAddr(t *testing.T) {
	reg := NewShardRegistry()
	fake := newRowCountFakeStore()
	reg.SetStore(fake)

	ds, err := shard.NewDuckDBShard("shard-rc-same", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg.Add(&ShardEntry{
		ID:              "shard-rc-same",
		Apps:            []string{"weft"},
		State:           StateIngesting,
		Addr:            "10.0.0.1:9000",
		Shard:           ds,
		RowCount:        0, // fresh shard at registration time
		BucketID:        1,
		CreatedAt:       time.Now(),
		LastHeartbeatAt: time.Now().Add(-3 * time.Minute),
	})

	prov := &Provisioner{
		registry:   reg,
		config:     ProvisionerConfig{PollInterval: time.Second, WindowDuration: 7 * 24 * time.Hour},
		configurer: &stubConfigurer{},
	}

	const live = int64(42_000)
	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:  "shard-rc-same",
		Addr:     "10.0.0.1:9000",
		State:    "INGESTING",
		RowCount: live,
	})

	got := reg.Get("shard-rc-same")
	if got == nil {
		t.Fatal("entry vanished after heartbeat")
	}
	if got.RowCount != live {
		t.Errorf("registry entry RowCount = %d, want %d (heartbeat dropped on the floor)", got.RowCount, live)
	}
	if persisted := fake.lastSaved("shard-rc-same"); persisted == nil {
		t.Fatal("entry was never persisted to the store after heartbeat update")
	} else if persisted.RowCount != live {
		t.Errorf("persisted RowCount = %d, want %d", persisted.RowCount, live)
	}
}

// TestProcessHeartbeat_UpdatesRowCount_Rebind covers the instance-
// refresh path: same shard id, new addr (the prior instance went
// away). The rebind already persists Addr/InstanceID; it must also
// lift hb.RowCount, otherwise every deploy resets the displayed
// count to whatever the new instance happens to report on its first
// heartbeat after restart (often 0 if the binary boots faster than
// it can recount).
func TestProcessHeartbeat_UpdatesRowCount_Rebind(t *testing.T) {
	reg := NewShardRegistry()
	fake := newRowCountFakeStore()
	reg.SetStore(fake)

	ds, err := shard.NewDuckDBShard("shard-rc-rebind", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg.Add(&ShardEntry{
		ID:              "shard-rc-rebind",
		Apps:            []string{"weft"},
		State:           StateIngesting,
		Addr:            "10.0.0.1:9000",
		InstanceID:      "i-old",
		Shard:           ds,
		RowCount:        100,
		BucketID:        1,
		CreatedAt:       time.Now(),
		LastHeartbeatAt: time.Now().Add(-30 * time.Second),
	})

	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			PollInterval:     time.Second,
			WindowDuration:   7 * 24 * time.Hour,
			ShardSecret:      "test-secret",
			CloseRebindGrace: 10 * time.Millisecond,
		},
		configurer: &stubConfigurer{},
	}

	const live = int64(99_999)
	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    "shard-rc-rebind",
		Addr:       "10.0.0.2:9000", // new address — triggers rebind
		InstanceID: "i-new",
		State:      "INGESTING",
		RowCount:   live,
	})

	got := reg.Get("shard-rc-rebind")
	if got == nil {
		t.Fatal("entry vanished after heartbeat")
	}
	if got.RowCount != live {
		t.Errorf("registry entry RowCount = %d, want %d (rebind dropped the count)", got.RowCount, live)
	}
	if persisted := fake.lastSaved("shard-rc-rebind"); persisted == nil {
		t.Fatal("entry was never persisted to the store after rebind")
	} else if persisted.RowCount != live {
		t.Errorf("persisted RowCount = %d, want %d", persisted.RowCount, live)
	}
}

// rowCountFakeStore is a minimal RegistryStore that records the most
// recent SaveShard payload per shard id. Lets the tests above assert
// that PersistState actually fires (and with the right RowCount)
// without needing a real postgres.
type rowCountFakeStore struct {
	saved map[string]*store.ShardRecord
}

func newRowCountFakeStore() *rowCountFakeStore {
	return &rowCountFakeStore{saved: map[string]*store.ShardRecord{}}
}

func (f *rowCountFakeStore) SaveShard(rec *store.ShardRecord) error {
	cp := *rec
	f.saved[rec.ID] = &cp
	return nil
}

func (f *rowCountFakeStore) DeleteShard(id string) error                          { return nil }
func (f *rowCountFakeStore) ListShards() ([]store.ShardRecord, error)             { return nil, nil }
func (f *rowCountFakeStore) GetShard(id string) (store.ShardRecord, bool, error)  { return store.ShardRecord{}, false, nil }
func (f *rowCountFakeStore) lastSaved(id string) *store.ShardRecord               { return f.saved[id] }
