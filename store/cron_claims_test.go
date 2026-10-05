package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaimCronSlotExactlyOnceUnderConcurrency(t *testing.T) {
	db, err := Open(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	key := fmt.Sprintf("test:%d:slot", time.Now().UnixNano())
	t.Cleanup(func() { db.db.Exec(`DELETE FROM cron_claims WHERE key = ?`, key) })

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := db.ClaimCronSlot(key, fmt.Sprintf("coord-%d", i))
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("want exactly 1 winner, got %d", wins.Load())
	}
}

func TestDeleteCronClaimsBefore(t *testing.T) {
	db := testOpen(t)
	for _, k := range []string{"cleanup:old", "cleanup:new"} {
		if ok, err := db.ClaimCronSlot(k, "me"); err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", k, ok, err)
		}
	}
	if err := db.db.Exec(`UPDATE cron_claims SET claimed_at = now() - interval '2 hours' WHERE key = 'cleanup:old'`).Error; err != nil {
		t.Fatal(err)
	}
	n, err := db.DeleteCronClaimsBefore(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("deleted %d, want >=1", n)
	}
	if ok, _ := db.ClaimCronSlot("cleanup:old", "me"); !ok {
		t.Fatal("old claim should have been removed")
	}
	if ok, _ := db.ClaimCronSlot("cleanup:new", "me"); ok {
		t.Fatal("recent claim should have been kept")
	}
}
