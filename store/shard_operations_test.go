package store

import (
	"database/sql"
	"testing"
)

// TestShardOperationsAuditTrigger pins the append-only shard-state
// audit log (migration 049). Every shard_records state change must
// land exactly one shard_operations row; a non-state UPDATE must land
// none; INSERT records (NULL -> state); DELETE records (state -> NULL)
// and must survive the parent row going away (no FK on shard_id).
func TestShardOperationsAuditTrigger(t *testing.T) {
	db := testOpen(t)
	g := db.db

	var bucketID int64
	if err := g.Raw(`SELECT id FROM buckets ORDER BY id LIMIT 1`).Scan(&bucketID).Error; err != nil || bucketID == 0 {
		t.Fatalf("need a seed bucket: id=%d err=%v", bucketID, err)
	}

	const sid = "shard-audit-test"
	ins := func(q string, args ...any) {
		t.Helper()
		if err := g.Exec(q, args...).Error; err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	type op struct {
		From sql.NullString
		To   sql.NullString
	}
	count := func() int {
		var n int
		if err := g.Raw(`SELECT count(*) FROM shard_operations WHERE shard_id = ?`, sid).Scan(&n).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	latest := func() op {
		var o op
		if err := g.Raw(`SELECT from_state, to_state FROM shard_operations
			WHERE shard_id = ? ORDER BY ts DESC, ctid DESC LIMIT 1`, sid).
			Row().Scan(&o.From, &o.To); err != nil {
			t.Fatalf("latest: %v", err)
		}
		return o
	}
	want := func(o op, from, to string) {
		t.Helper()
		gotFrom := "NULL"
		if o.From.Valid {
			gotFrom = o.From.String
		}
		gotTo := "NULL"
		if o.To.Valid {
			gotTo = o.To.String
		}
		if gotFrom != from || gotTo != to {
			t.Fatalf("latest op = (%s -> %s), want (%s -> %s)", gotFrom, gotTo, from, to)
		}
	}

	// INSERT -> (NULL -> LAUNCHING)
	ins(`INSERT INTO shard_records (id, window_start, window_end, state, created_at, bucket_id)
	     VALUES (?, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', 'LAUNCHING', now(), ?)`, sid, bucketID)
	if count() != 1 {
		t.Fatalf("after INSERT: count=%d want 1", count())
	}
	want(latest(), "NULL", "LAUNCHING")

	// state UPDATE -> (LAUNCHING -> INGESTING)
	ins(`UPDATE shard_records SET state = 'INGESTING' WHERE id = ?`, sid)
	if count() != 2 {
		t.Fatalf("after state UPDATE: count=%d want 2", count())
	}
	want(latest(), "LAUNCHING", "INGESTING")

	// non-state UPDATE -> no new row
	ins(`UPDATE shard_records SET row_count = 99 WHERE id = ?`, sid)
	if count() != 2 {
		t.Fatalf("after row_count-only UPDATE: count=%d want 2 (no audit row for non-state change)", count())
	}

	// DELETE -> (INGESTING -> NULL), and the audit row outlives the parent
	ins(`DELETE FROM shard_records WHERE id = ?`, sid)
	if count() != 3 {
		t.Fatalf("after DELETE: count=%d want 3", count())
	}
	want(latest(), "INGESTING", "NULL")
}
