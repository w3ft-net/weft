package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

// Parallel of version_history_test.go for cron_watches.

func cronHistoryCount(t *testing.T, db *Store, customerID uint, name string) int {
	t.Helper()
	var n int64
	err := db.db.Raw(`
		SELECT COUNT(*) FROM cron_watch_history h
		JOIN cron_watches w ON w.id = h.watch_id
		WHERE w.customer_id = ? AND w.name = ?
	`, customerID, name).Scan(&n).Error
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
	cust, _ := db.FindOrCreateCustomer("cvh-first")
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
	cust, _ := db.FindOrCreateCustomer("cvh-noop")
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
	cust, _ := db.FindOrCreateCustomer("cvh-update")
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
		WHERE w.customer_id = ? AND w.name = ?
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
	cust, _ := db.FindOrCreateCustomer("cvh-cap")
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
	cust, _ := db.FindOrCreateCustomer("cvh-schedchange")
	user, _ := db.CreateUser("alice-cron-sched@example.test")

	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 5m", "echo 1", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo 1", nil)

	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != 1 {
		t.Errorf("schedule-only change: got %d history rows, want 1", got)
	}
}

func TestLogCronWatch_OrderAndShape(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-log")
	user, _ := db.CreateUser("alice-cron-log@example.test")

	saves := []struct{ s, p string }{
		{"every 5m", "echo v1"},
		{"every 5m", "echo v2"},
		{"every 1m", "echo v3"},
	}
	for _, s := range saves {
		if err := db.SaveCronWatch(cust.ID, user.ID, "errors", s.s, s.p, nil); err != nil {
			t.Fatal(err)
		}
	}

	versions, err := db.LogCronWatch(cust.ID, "errors")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("Log: got %d versions, want 3", len(versions))
	}
	if versions[0].Pipeline != "echo v3" || !versions[0].IsHead {
		t.Errorf("HEAD: got pipeline=%q IsHead=%v, want v3 head", versions[0].Pipeline, versions[0].IsHead)
	}
	if versions[2].Pipeline != "echo v1" || versions[2].Schedule != "every 5m" {
		t.Errorf("oldest: got pipeline=%q schedule=%q, want v1/every 5m",
			versions[2].Pipeline, versions[2].Schedule)
	}
}

func TestResolveCronWatchRef_HEAD(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-resolve-head")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo current", nil)

	v, err := db.ResolveCronWatchRef(cust.ID, "errors", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if v.Pipeline != "echo current" || !v.IsHead {
		t.Errorf("HEAD: got pipeline=%q IsHead=%v", v.Pipeline, v.IsHead)
	}
}

func TestResolveCronWatchRef_HEADCaret(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-resolve-caret")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo old", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo new", nil)

	v, err := db.ResolveCronWatchRef(cust.ID, "errors", "HEAD^")
	if err != nil {
		t.Fatal(err)
	}
	if v.Pipeline != "echo old" {
		t.Errorf("HEAD^: got pipeline=%q, want echo old", v.Pipeline)
	}
}

func TestResolveCronWatchRef_HashPrefix(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-resolve-hash")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo target", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo other", nil)

	target := cronHashOf("every 1m", "echo target")
	v, err := db.ResolveCronWatchRef(cust.ID, "errors", target[:8])
	if err != nil {
		t.Fatal(err)
	}
	if v.Pipeline != "echo target" {
		t.Errorf("by hash prefix: got pipeline=%q, want target", v.Pipeline)
	}
}

func TestResolveCronWatchRef_UnknownHash(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-resolve-unknown")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo here", nil)

	if _, err := db.ResolveCronWatchRef(cust.ID, "errors", "deadbeef"); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("unknown hash: got %v, want ErrRefNotFound", err)
	}
}

func TestCheckoutCronWatch_RoundTrip(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-checkout")
	alice, _ := db.CreateUser("alice@example.test")
	bob, _ := db.CreateUser("bob@example.test")

	db.SaveCronWatch(cust.ID, alice.ID, "errors", "every 5m", "echo v1", nil)
	db.SaveCronWatch(cust.ID, alice.ID, "errors", "every 5m", "echo v2", nil)
	db.SaveCronWatch(cust.ID, bob.ID, "errors", "every 1m", "echo v3", nil)

	if err := checkoutCronWatch(db, cust.ID, bob.ID, "errors", "HEAD^2"); err != nil {
		t.Fatal(err)
	}
	w, _ := db.GetCronWatch(cust.ID, "errors")
	if w.Pipeline != "echo v1" || w.Schedule != "every 5m" {
		t.Errorf("after checkout: got schedule=%q pipeline=%q, want every 5m / echo v1",
			w.Schedule, w.Pipeline)
	}
	if w.CreatedByUserID == nil || *w.CreatedByUserID != alice.ID {
		t.Errorf("checkout should preserve original author; got %v, want alice", w.CreatedByUserID)
	}
}

func TestAmendCronWatchMessage_HEAD_DoesNotCapture(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-amend-head")
	user, _ := db.CreateUser("alice@example.test")

	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo v1", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "echo v2", nil)
	before := cronHistoryCount(t, db, cust.ID, "errors")

	if err := db.AmendCronWatchMessage(cust.ID, "errors", "HEAD", "tighten window"); err != nil {
		t.Fatal(err)
	}
	if got := cronHistoryCount(t, db, cust.ID, "errors"); got != before {
		t.Errorf("amend should NOT create a history row; before=%d after=%d", before, got)
	}
	v, _ := db.ResolveCronWatchRef(cust.ID, "errors", "HEAD")
	if v.Message != "tighten window" {
		t.Errorf("message: got %q, want %q", v.Message, "tighten window")
	}
}

func TestAmendCronWatchMessage_HistoryRef(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("cvh-amend-history")
	user, _ := db.CreateUser("alice@example.test")

	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "v1", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "v2", nil)
	db.SaveCronWatch(cust.ID, user.ID, "errors", "every 1m", "v3", nil)

	if err := db.AmendCronWatchMessage(cust.ID, "errors", "HEAD^2", "first version"); err != nil {
		t.Fatal(err)
	}
	v, _ := db.ResolveCronWatchRef(cust.ID, "errors", "HEAD^2")
	if v.Message != "first version" {
		t.Errorf("HEAD^2 message: got %q, want %q", v.Message, "first version")
	}
}

// checkoutCronWatch mirrors the former Store.CheckoutCronWatch, whose
// production logic moved to a caller outside the store package.
func checkoutCronWatch(db *Store, customerID uint, userID, name, ref string) error {
	target, err := db.ResolveCronWatchRef(customerID, name, ref)
	if err != nil {
		return err
	}
	if target.IsHead {
		return nil
	}
	compiled, err := CompileWatchPipeline(target.Pipeline)
	if err != nil {
		return err
	}
	return db.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"schedule":        target.Schedule,
			"pipeline":        target.Pipeline,
			"content_hash":    target.ContentHash,
			"message":         target.Message,
			"alert_key":       compiled.AlertKey,
			"alert_tag":       compiled.AlertTag,
			"alert_condition": compiled.AlertCondition,
			"alert_threshold": compiled.AlertThreshold,
			"updated_at":      time.Now(),
		}
		if target.AuthoredByUserID != nil {
			updates["created_by_user_id"] = *target.AuthoredByUserID
		}
		return tx.Model(&CronWatch{}).
			Where("name = ?", name).
			Updates(updates).Error
	})
}
