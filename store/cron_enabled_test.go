package store

import (
	"context"
	"testing"
)

func TestSaveCronWatch_EnabledFlag(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("cron-enabled")
	save := func(enabled bool) {
		t.Helper()
		if err := db.SaveCronWatch(org.ID, "", "w", "every 5m", "true", enabled, nil); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (enabled bool, count int) {
		t.Helper()
		w, err := db.GetCronWatch(org.ID, "w")
		if err != nil {
			t.Fatal(err)
		}
		count, err = db.CountCronWatches(org.ID)
		if err != nil {
			t.Fatal(err)
		}
		return w.Enabled, count
	}

	save(false)
	if en, n := state(); en || n != 0 {
		t.Fatalf("new disabled cron: enabled=%v count=%d, want false/0", en, n)
	}
	// Same schedule and pipeline: the content-hash no-op must not swallow the flip.
	save(true)
	if en, n := state(); !en || n != 1 {
		t.Fatalf("re-enabled: enabled=%v count=%d, want true/1", en, n)
	}
	save(false)
	if en, n := state(); en || n != 0 {
		t.Fatalf("disabled again: enabled=%v count=%d, want false/0", en, n)
	}
}

func TestRecordCronTimeout_BenchesAtThreshold(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("cron-bench")
	if err := db.SaveCronWatch(org.ID, "", "slow", "every 5m", "true", true, nil); err != nil {
		t.Fatal(err)
	}
	w, _ := db.GetCronWatch(org.ID, "slow")

	for i := 1; i <= 3; i++ {
		benched, err := db.RecordCronTimeout(context.Background(), w.ID, 3)
		if err != nil {
			t.Fatal(err)
		}
		if benched != (i == 3) {
			t.Fatalf("strike %d: benched=%v", i, benched)
		}
	}
	got, _ := db.GetCronWatch(org.ID, "slow")
	if got.Enabled {
		t.Fatal("cron still enabled after three strikes")
	}

	// Re-enabling through a save wipes the slate.
	if err := db.SaveCronWatch(org.ID, "", "slow", "every 5m", "true", true, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetCronWatch(org.ID, "slow")
	if !got.Enabled || got.ConsecutiveTimeouts != 0 {
		t.Fatalf("after re-enable: enabled=%v strikes=%d, want true/0", got.Enabled, got.ConsecutiveTimeouts)
	}
}
