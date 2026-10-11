package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Parallel of version_history_test.go for cron_watches.

func cronHistoryCount(t *testing.T, db *Store, orgID OrgID, name string) int {
	t.Helper()
	var n int64
	err := db.db.Raw(`
		SELECT COUNT(*) FROM cron_watch_history h
		JOIN cron_watches w ON w.id = h.watch_id
		WHERE w.org_id = ? AND w.name = ?
	`, orgID, name).Scan(&n).Error
	if err != nil {
		t.Fatalf("cronHistoryCount: %v", err)
	}
	return int(n)
}

func cronHashOf(schedule, pipeline string) string {
	h := sha256.Sum256([]byte(schedule + "\n" + pipeline))
	return hex.EncodeToString(h[:])
}

func TestCronWatch_FirstSaveCreatesNoHistory(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cvh-first")
	user, _ := db.CreateUser("alice-cron-first@example.test")

	if err := db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo 1", nil); err != nil {
		t.Fatal(err)
	}
	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 0 {
		t.Errorf("first save should produce no history rows; got %d", got)
	}
}

func TestCronWatch_IdenticalResaveDoesNotCapture(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cvh-noop")
	user, _ := db.CreateUser("alice-cron-noop@example.test")

	if err := db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo 1", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo 1", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 0 {
		t.Errorf("identical re-saves should produce no history rows; got %d", got)
	}

	if err := db.SaveCronWatch(cust.ID, user.ID, "errors", "every 5m", "echo 1", nil); err != nil {
		t.Fatal(err)
	}
	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 1 {
		t.Errorf("real change after no-ops: got %d rows, want 1", got)
	}
}

func TestCronWatch_UpdateCapturesPriorVersion(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cvh-update")
	alice, _ := db.CreateUser("alice-cron-up@example.test")
	bob, _ := db.CreateUser("bob-cron-up@example.test")

	if err := db.SaveCronWatch(cust.ID, alice.ID, "errors", "every 5m", "echo 1", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, bob.ID, "errors", "every 1m", "echo 2", nil); err != nil {
		t.Fatal(err)
	}

	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 1 {
		t.Fatalf("after overwrite: got %d history rows, want 1", got)
	}

	type row struct {
		Schedule         string
		Pipeline         string
		ContentHash      string
		AuthoredByUserID *string
		CapturedByUserID *string
	}
	var r row
	err := db.db.Raw(`
		SELECT h.schedule, h.pipeline, h.content_hash, h.authored_by_user_id, h.captured_by_user_id
		FROM cron_watch_history h
		JOIN cron_watches w ON w.id = h.watch_id
		WHERE w.org_id = ? AND w.name = ?
	`, cust.ID, "errors").Scan(&r).Error
	if err != nil {
		t.Fatal(err)
	}

	if r.Schedule != "every 5m" || r.Pipeline != "echo 1" {
		t.Errorf("captured: got schedule=%q pipeline=%q, want (every 5m, echo 1)",
			r.Schedule, r.Pipeline)
	}
	if r.ContentHash != cronHashOf("every 5m", "echo 1") {
		t.Errorf("content_hash: got %q, want %q", r.ContentHash, cronHashOf("every 5m", "echo 1"))
	}
	if r.AuthoredByUserID == nil || *r.AuthoredByUserID != alice.ID {
		t.Errorf("authored_by: got %v, want alice", r.AuthoredByUserID)
	}
	if r.CapturedByUserID == nil || *r.CapturedByUserID != bob.ID {
		t.Errorf("captured_by: got %v, want bob", r.CapturedByUserID)
	}
}

func TestCronWatch_FiveCapDropsOldest(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cvh-cap")
	user, _ := db.CreateUser("alice-cron-cap@example.test")

	for i := 1; i <= 7; i++ {
		pipe := "echo v" + string(rune('0'+i))
		if err := db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", pipe, nil); err != nil {
			t.Fatal(err)
		}
	}

	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 5 {
		t.Errorf("after 6 updates: got %d history rows, want 5 (cap)", got)
	}
}

func TestCronWatch_ScheduleOnlyChangeCapturesHistory(t *testing.T) {
	// Schedule is part of the content hash + a content column on the
	// trigger's UPDATE OF clause, so changing only the schedule
	// (without touching pipeline) still captures the prior version.
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cvh-schedchange")
	user, _ := db.CreateUser("alice-cron-sched@example.test")

	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 5m", "echo 1", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo 1", nil)

	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 1 {
		t.Errorf("schedule-only change: got %d history rows, want 1", got)
	}
}

func TestAmendCronWatchMessage_HEAD_DoesNotCapture(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cvh-amend-head")
	user, _ := db.CreateUser("alice@example.test")

	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo v1", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo v2", nil)
	before := cronHistoryCount(t, db, cust.ID, "errors")

	if err := db.AmendCronWatchMessage(cust.ID, "errors", "tighten window"); err != nil {
		t.Fatal(err)
	}
	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != before {
		t.Errorf("amend should NOT create a history row; before=%d after=%d", before, got)
	}
	v, _ := db.GetCronWatch(cust.ID, "errors")
	if v.Message != "tighten window" {
		t.Errorf("message: got %q, want %q", v.Message, "tighten window")
	}
}
