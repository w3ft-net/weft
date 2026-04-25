package store

import (
	"testing"
)

// TestBackfillAlertThresholds_PopulatesExistingRows simulates the
// post-027 state: a watch exists with a pipeline that has an
// `alert above N` stage, but its alert_condition/alert_threshold
// columns are still NULL (as they would be for rows inserted before
// SaveCronWatch learned to populate them). After calling the
// backfill, both columns should be set to the values compiled from
// the pipeline.
func TestBackfillAlertThresholds_PopulatesExistingRows(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("backfill-test")
	if err != nil {
		t.Fatal(err)
	}

	// Insert a watch and deliberately NULL the new columns so the
	// backfill has something to do.
	if err := db.SaveCronWatch(cust.ID, "", "with-threshold",
		"*/5 * * * *", "echo 1 | alert above 42"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, "", "no-threshold",
		"*/5 * * * *", "echo 1 | alert changed"); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`
		UPDATE cron_watches
		SET alert_condition = NULL, alert_threshold = NULL
		WHERE customer_id = ?`, cust.ID,
	).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.backfillAlertThresholds(); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Watch with `above 42` should have both columns populated.
	w, err := db.GetCronWatch(cust.ID, "with-threshold")
	if err != nil {
		t.Fatal(err)
	}
	if w.AlertCondition == nil || *w.AlertCondition != "above" {
		t.Errorf("with-threshold: AlertCondition = %v, want \"above\"", derefStr(w.AlertCondition))
	}
	if w.AlertThreshold == nil || *w.AlertThreshold != 42 {
		t.Errorf("with-threshold: AlertThreshold = %v, want 42", derefF64(w.AlertThreshold))
	}

	// Watch with `changed` has no numeric threshold; backfill must
	// leave both columns NULL rather than inventing a value.
	w2, err := db.GetCronWatch(cust.ID, "no-threshold")
	if err != nil {
		t.Fatal(err)
	}
	if w2.AlertCondition != nil {
		t.Errorf("no-threshold: AlertCondition = %v, want nil", *w2.AlertCondition)
	}
	if w2.AlertThreshold != nil {
		t.Errorf("no-threshold: AlertThreshold = %v, want nil", *w2.AlertThreshold)
	}

	// Second run is a no-op: nothing left with NULL+NULL for the
	// populated row, and the other row stays NULL.
	if err := db.backfillAlertThresholds(); err != nil {
		t.Fatalf("second backfill run: %v", err)
	}
	w, _ = db.GetCronWatch(cust.ID, "with-threshold")
	if w.AlertCondition == nil || *w.AlertCondition != "above" || *w.AlertThreshold != 42 {
		t.Errorf("second run mutated the populated row: %+v / %+v", w.AlertCondition, w.AlertThreshold)
	}
}
