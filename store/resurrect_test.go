package store

import "testing"

// Deleting is a soft delete; re-saving the same name must bring the row
// back (history continues) instead of colliding on the unique index.
func TestShellFunction_SaveAfterDeleteResurrects(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("resurrect-bin")
	user, _ := db.CreateUser("alice-resurrect-bin@example.test")

	for _, src := range []string{"echo 1", "echo 2"} {
		if err := db.SaveShellFunction(cust.ID, user.ID, "x", src, "bash"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteShellFunction(cust.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(cust.ID, user.ID, "x", "echo 3", "bash"); err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
	fn, err := db.GetShellFunction(cust.ID, "x")
	if err != nil || fn.Source != "echo 3" {
		t.Fatalf("get = %+v, %v", fn, err)
	}
	if got := historyCount(t, db, cust.ID, "x"); got != 2 {
		t.Errorf("history rows = %d, want 2 (history continues across delete)", got)
	}
}

// Re-saving the exact content a script had at deletion still revives it.
func TestShellFunction_SameContentAfterDeleteResurrects(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("resurrect-same")
	if err := db.SaveShellFunction(cust.ID, "", "x", "echo 1", "bash"); err != nil {
		t.Fatal(err)
	}
	_ = db.DeleteShellFunction(cust.ID, "x")
	if err := db.SaveShellFunction(cust.ID, "", "x", "echo 1", "bash"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetShellFunction(cust.ID, "x"); err != nil {
		t.Errorf("still deleted: %v", err)
	}
}

func TestCronWatch_SaveAfterDeleteResurrects(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("resurrect-cron")
	if err := db.SaveCronWatch(cust.ID, "", "t", "*/10 * * * *", "echo a", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCronWatch(cust.ID, "t"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, "", "t", "*/10 * * * *", "echo b", true, nil); err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
	w, err := db.GetCronWatch(cust.ID, "t")
	if err != nil || w.Pipeline != "echo b" || !w.Enabled {
		t.Fatalf("get = %+v, %v", w, err)
	}
}
