package service

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
)

func TestAuditFleet_ReportsRowsSparesAndErrorCount(t *testing.T) {
	st, done := testStore(t)
	defer done()
	store := logdb.ShardStore{Store: st}

	before, err := store.IngestionErrorCount()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Exec(`INSERT INTO ingestion_errors (shard_id, error_msg, row_count, apps, customers) VALUES ('s', 'e', 1, '{}', '{}')`).Error; err != nil {
		t.Fatal(err)
	}
	claimer := "s-claim"
	if err := store.InsertSpare(&logdb.Spare{InstanceID: "i-free", Tier: "small", AZ: "a", LaunchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertSpare(&logdb.Spare{InstanceID: "i-held", Tier: "small", AZ: "a", LaunchedAt: time.Now(), ClaimedBy: &claimer}); err != nil {
		t.Fatal(err)
	}

	resp, err := (&Server{Store: st}).AuditFleet(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.IngestionErrors != before+1 {
		t.Fatalf("ingestion_errors = %d, want %d", resp.IngestionErrors, before+1)
	}
	claimed := map[string]bool{}
	for _, sp := range resp.Spares {
		claimed[sp.InstanceId] = sp.Claimed
	}
	if free, ok := claimed["i-free"]; !ok || free {
		t.Fatalf("i-free: present=%v claimed=%v, want present unclaimed", ok, free)
	}
	if !claimed["i-held"] {
		t.Fatal("i-held should be reported claimed")
	}
}
