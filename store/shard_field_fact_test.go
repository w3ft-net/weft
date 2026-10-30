package store

import "testing"

func TestShardFieldFacts_UpsertDropAndCascade(t *testing.T) {
	db := testOpen(t)
	if err := db.DB().Exec(`INSERT INTO shard_records (id, bucket_id, window_start, window_end, state, created_at)
		VALUES ('ff-shard', (SELECT id FROM buckets WHERE label = 'default'), now(), now(), 'INGESTING', now())`).Error; err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	t.Cleanup(func() { db.DB().Exec(`DELETE FROM shard_records WHERE id = 'ff-shard'`) })

	a := ShardFieldFact{CustomerID: 1, Namespace: "ns", Service: "svc", Kind: "main", Env: "prod", Locator: "$5", Epoch: 3}
	b := a
	b.Locator = "$6"
	if err := db.ApplyShardFieldFacts("ff-shard", []ShardFieldFact{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	// Upserting a again moves its epoch rather than conflicting.
	a.Epoch = 8
	if err := db.ApplyShardFieldFacts("ff-shard", []ShardFieldFact{a}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyShardFieldFacts("ff-shard", nil, []ShardFieldFact{b}); err != nil {
		t.Fatal(err)
	}
	rows := factsOf(t, db, "ff-shard")
	if len(rows) != 1 || rows[0].Locator != "$5" || rows[0].Epoch != 8 {
		t.Fatalf("rows = %+v, want only $5 at epoch 8", rows)
	}

	if err := db.DB().Exec(`DELETE FROM shard_records WHERE id = 'ff-shard'`).Error; err != nil {
		t.Fatal(err)
	}
	if rows := factsOf(t, db, "ff-shard"); len(rows) != 0 {
		t.Errorf("facts outlived their shard: %+v", rows)
	}
}

func factsOf(t *testing.T, db *Store, shardID string) []ShardFieldFact {
	t.Helper()
	all, err := db.ListShardFieldFacts()
	if err != nil {
		t.Fatal(err)
	}
	var out []ShardFieldFact
	for _, f := range all {
		if f.ShardID == shardID {
			out = append(out, f)
		}
	}
	return out
}
