package store

import (
	"testing"
	"time"
)

func TestCronRunTimeout_PerOrgOverride(t *testing.T) {
	db := testOpen(t)
	a, _ := db.FindOrCreateOrg("run-timeout-a")
	b, _ := db.FindOrCreateOrg("run-timeout-b")

	if d, err := db.CronRunTimeout(a.ID); err != nil || d != 0 {
		t.Fatalf("unset: d=%v err=%v, want 0 (use the default)", d, err)
	}
	if err := db.SetCronRunTimeout(a.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.CronRunTimeout(a.ID); d != 10*time.Second {
		t.Errorf("a = %v, want 10s", d)
	}
	if d, _ := db.CronRunTimeout(b.ID); d != 0 {
		t.Errorf("b = %v, want it untouched", d)
	}
	if err := db.SetCronRunTimeout(a.ID, 0); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.CronRunTimeout(a.ID); d != 0 {
		t.Errorf("after clearing: %v, want 0", d)
	}
	if err := db.SetCronRunTimeout(a.ID, -time.Second); err == nil {
		t.Error("negative budget accepted")
	}
}
