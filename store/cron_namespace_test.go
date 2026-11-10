package store

import "testing"

// A cron lives in a namespace, or at the org level. The same name can exist in
// each namespace and at the org level without colliding.
func TestCronWatch_HomeNamespaceScopesTheName(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("cron-home")
	alpha, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	beta, _ := db.FindOrCreateNamespace(org.ID, "beta")

	for _, ns := range []uint{alpha.ID, beta.ID, 0} {
		if err := db.SaveCronWatch(org.ID, ns, "", "errors", "every 5m", "echo 1", true, nil); err != nil {
			t.Fatalf("save in %d: %v", ns, err)
		}
	}
	if err := db.SaveCronWatch(org.ID, alpha.ID, "", "errors", "every 10m", "echo 2", true, nil); err != nil {
		t.Fatalf("resave: %v", err)
	}

	got, err := db.GetCronWatch(org.ID, alpha.ID, "errors")
	if err != nil || got.Schedule != "every 10m" || got.NamespaceID == nil || *got.NamespaceID != alpha.ID {
		t.Fatalf("alpha/errors = %+v, %v", got, err)
	}
	got, err = db.GetCronWatch(org.ID, 0, "errors")
	if err != nil || got.Schedule != "every 5m" || got.NamespaceID != nil {
		t.Fatalf("org-level errors = %+v, %v", got, err)
	}
	all, _ := db.ListCronWatches(org.ID)
	if len(all) != 3 {
		t.Fatalf("listed %d crons, want 3", len(all))
	}

	if err := db.DeleteCronWatch(org.ID, beta.ID, "errors"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCronWatch(org.ID, beta.ID, "errors"); err == nil {
		t.Error("beta/errors survived its delete")
	}
	if _, err := db.GetCronWatch(org.ID, alpha.ID, "errors"); err != nil {
		t.Errorf("deleting beta/errors removed alpha/errors: %v", err)
	}
}
