package store

import "testing"

// Regression: `cat /cron.d/<name>` and `ls /cron.d` were returning
// nothing for watches whose `alert` stage uses .json.path selectors
// (-v .pct -t .host), while watches without selectors (or with
// positional ones) round-trip fine. Both Get and List failed in the
// running coordinator after the multi-customer demo seed; only the
// alert-selector shape distinguishes the two seeded watches.
//
// Reproduces with the literal pipeline shape used by mem-pressure.
func TestGetCronWatch_JSONPathAlertSelectors(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cron-jsonpath")
	user, _ := db.CreateUser("alice-jp@example.test")

	pipeline := `tail -n 12 /log/x/y_metrics.log | jq 'select(.type == "memory")' | alert above 70 -v .pct -t .host`
	if err := db.SaveCronWatch(cust.ID, 0, user.ID, "mem-pressure", "*/1 * * * *", pipeline, true, nil); err != nil {
		t.Fatalf("SaveCronWatch: %v", err)
	}

	w, err := db.GetCronWatch(cust.ID, 0, "mem-pressure")
	if err != nil {
		t.Fatalf("GetCronWatch: %v (want round-trip to succeed)", err)
	}
	if w.Name != "mem-pressure" {
		t.Errorf("round-tripped name: got %q, want mem-pressure", w.Name)
	}
	if w.Pipeline != pipeline {
		t.Errorf("round-tripped pipeline: got %q, want %q", w.Pipeline, pipeline)
	}
	if w.ValueField == nil || *w.ValueField != ".pct" {
		t.Errorf("AlertKey: got %v, want \".pct\"", w.ValueField)
	}
	if w.TagField == nil || *w.TagField != ".host" {
		t.Errorf("AlertTag: got %v, want \".host\"", w.TagField)
	}

	all, err := db.ListCronWatches(cust.ID)
	if err != nil {
		t.Fatalf("ListCronWatches: %v (want list to succeed)", err)
	}
	if len(all) != 1 {
		t.Fatalf("ListCronWatches: got %d rows, want 1 (\"mem-pressure\" missing)", len(all))
	}
}
