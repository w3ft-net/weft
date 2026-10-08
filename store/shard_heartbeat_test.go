package store

import (
	"testing"
	"time"
)

func heartbeatRowCount(t *testing.T, db *Store) int {
	t.Helper()
	var n int
	if err := db.db.Raw(`SELECT count(*) FROM shard_heartbeats`).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// setBeatAt backdates a source's row. RecordHeartbeat stamps NOW(), which is
// constant inside the test transaction, so ordering and expiry need explicit times.
func setBeatAt(t *testing.T, db *Store, shardID, instanceID string, at time.Time) {
	t.Helper()
	if err := db.db.Exec(`UPDATE shard_heartbeats SET beat_at = ? WHERE shard_id = ? AND instance_id = ?`,
		at, shardID, instanceID).Error; err != nil {
		t.Fatal(err)
	}
}

// The table holds the latest beat per source, not a log of every beat: it is
// read only for "who is alive right now", so history is pure bloat.
func TestRecordHeartbeat_KeepsOneRowPerSource(t *testing.T) {
	db := testOpen(t)

	for i := int64(1); i <= 5; i++ {
		hb := &ShardHeartbeat{ShardID: "s-a", Addr: "10.0.0.1:9000", InstanceID: "i-1", State: "INGESTING", RowCount: i * 10, DiskBytes: i * 100}
		if err := db.RecordHeartbeat(hb); err != nil {
			t.Fatalf("beat %d: %v", i, err)
		}
	}
	if n := heartbeatRowCount(t, db); n != 1 {
		t.Fatalf("5 beats from one source left %d rows, want 1", n)
	}

	live, err := db.LiveShardHeartbeats("1 minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].RowCount != 50 || live[0].DiskBytes != 500 {
		t.Fatalf("live = %+v, want the 5th beat (row_count 50, disk_bytes 500)", live)
	}
}

// A claimed shard that moves to a replacement instance leaves its old row
// behind until pruned. Reading both would replay the dead instance's addr at
// the registry, so only the newest beat per shard may come back.
func TestLiveShardHeartbeats_ReplacedInstanceReturnsNewestOnly(t *testing.T) {
	db := testOpen(t)
	now := time.Now()

	if err := db.RecordHeartbeat(&ShardHeartbeat{ShardID: "s-a", Addr: "10.0.0.1:9000", InstanceID: "i-old", State: "INGESTING"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHeartbeat(&ShardHeartbeat{ShardID: "s-a", Addr: "10.0.0.2:9000", InstanceID: "i-new", State: "INGESTING"}); err != nil {
		t.Fatal(err)
	}
	setBeatAt(t, db, "s-a", "i-old", now.Add(-30*time.Second))
	setBeatAt(t, db, "s-a", "i-new", now.Add(-5*time.Second))

	live, err := db.LiveShardHeartbeats("1 minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].InstanceID != "i-new" || live[0].Addr != "10.0.0.2:9000" {
		t.Fatalf("live = %+v, want only the replacement instance's beat", live)
	}
}

// Unclaimed instances beat with an empty shard id and are told apart by
// instance id. Collapsing them to one row would starve all but the newest
// during a multi-shard launch.
func TestLiveShardHeartbeats_UnclaimedInstancesAreAllReturned(t *testing.T) {
	db := testOpen(t)

	for _, inst := range []string{"i-aaa", "i-bbb"} {
		if err := db.RecordHeartbeat(&ShardHeartbeat{ShardID: "", Addr: inst + ":9000", InstanceID: inst, State: "UNCLAIMED"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := heartbeatRowCount(t, db); n != 2 {
		t.Fatalf("two unclaimed instances left %d rows, want 2", n)
	}
	live, err := db.LiveShardHeartbeats("1 minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("live = %+v, want both unclaimed instances", live)
	}
}

func TestLiveShardHeartbeats_ExcludesBeatsOutsideTheWindow(t *testing.T) {
	db := testOpen(t)
	if err := db.RecordHeartbeat(&ShardHeartbeat{ShardID: "s-a", Addr: "a", InstanceID: "i-1", State: "INGESTING"}); err != nil {
		t.Fatal(err)
	}
	setBeatAt(t, db, "s-a", "i-1", time.Now().Add(-2*time.Minute))

	live, err := db.LiveShardHeartbeats("1 minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live = %+v, want none (the only beat is 2 minutes old)", live)
	}
}

func TestPruneShardHeartbeats_DropsOnlyExpiredRows(t *testing.T) {
	db := testOpen(t)
	for _, src := range []struct{ shard, inst string }{{"s-old", "i-1"}, {"s-new", "i-2"}} {
		if err := db.RecordHeartbeat(&ShardHeartbeat{ShardID: src.shard, Addr: "a", InstanceID: src.inst, State: "INGESTING"}); err != nil {
			t.Fatal(err)
		}
	}
	setBeatAt(t, db, "s-old", "i-1", time.Now().Add(-3*time.Hour))
	setBeatAt(t, db, "s-new", "i-2", time.Now().Add(-5*time.Minute))

	if err := db.PruneShardHeartbeats(time.Hour); err != nil {
		t.Fatal(err)
	}
	var left []string
	if err := db.db.Raw(`SELECT shard_id FROM shard_heartbeats`).Scan(&left).Error; err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != "s-new" {
		t.Fatalf("after prune(1h) rows = %v, want only s-new", left)
	}
}

// The table is not partitioned or partman-managed any more: a hourly-partition
// maintenance job on a table this small is what wedged prod (default partition
// grew to 1 GB once maintenance started failing).
func TestShardHeartbeatsIsAPlainTable(t *testing.T) {
	db := testOpen(t)

	var kind string
	if err := db.db.Raw(`SELECT relkind::text FROM pg_class WHERE oid = 'public.shard_heartbeats'::regclass`).Scan(&kind).Error; err != nil {
		t.Fatal(err)
	}
	if kind != "r" {
		t.Fatalf("shard_heartbeats relkind = %q, want \"r\" (plain table)", kind)
	}
	var managed int
	if err := db.db.Raw(`SELECT count(*) FROM partman.part_config WHERE parent_table = 'public.shard_heartbeats'`).Scan(&managed).Error; err != nil {
		t.Fatal(err)
	}
	if managed != 0 {
		t.Fatal("shard_heartbeats is still registered with partman")
	}
}

// Replay needs to know how stale a beat is; the database measures that on its
// own clock so the coordinator's clock never enters into it.
func TestLiveShardHeartbeats_ReportsTheBeatsAge(t *testing.T) {
	db := testOpen(t)
	if err := db.RecordHeartbeat(&ShardHeartbeat{ShardID: "s-a", Addr: "a", InstanceID: "i-1", State: "INGESTING"}); err != nil {
		t.Fatal(err)
	}
	setBeatAt(t, db, "s-a", "i-1", time.Now().Add(-30*time.Second))

	live, err := db.LiveShardHeartbeats("1 minute")
	if err != nil || len(live) != 1 {
		t.Fatalf("live = %+v err=%v", live, err)
	}
	if live[0].Age < 29*time.Second || live[0].Age > 35*time.Second {
		t.Fatalf("Age = %v, want ~30s", live[0].Age)
	}
}
