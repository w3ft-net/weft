package retention

import (
	"fmt"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/store"
)

func newStore(db *store.Store) Store { return Store{logdb.ShardStore{Store: db}} }

// openTestStore is a transaction-scoped store, rolled back when the test ends.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, rollback := db.BeginTest()
	t.Cleanup(rollback)
	return tx
}

// seedBucket inserts a bucket row: shard_records carries a NOT NULL FK on
// bucket_id.
func seedBucket(t *testing.T, db *store.Store) *store.Bucket {
	t.Helper()
	b := &store.Bucket{
		Label:                fmt.Sprintf("ret-test-%d", time.Now().UnixNano()),
		Tier:                 "small",
		TargetIngestingCount: 2,
		CreatedAt:            "2026-05-21T12:00:00Z",
		WindowDuration:       "1h0m0s",
		State:                string(logdb.BucketStasis),
	}
	if err := db.SaveBucket(b); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	return b
}
