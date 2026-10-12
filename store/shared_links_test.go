package store

import "testing"

func TestSharedLink_SlugIsShortAndStable(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("sl-stable")
	user, _ := db.CreateUser("alice-sl@example.test")

	a, err := db.SaveSharedLink(org.ID, user.ID, "/bin/scan", "1h..", "--app=loom")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 10 {
		t.Errorf("slug %q: want 10 chars", a)
	}
	b, _ := db.SaveSharedLink(org.ID, user.ID, "/bin/scan", "1h..", "--app=loom")
	if a != b {
		t.Errorf("same payload gave two slugs: %q vs %q", a, b)
	}
	c, _ := db.SaveSharedLink(org.ID, user.ID, "/bin/scan", "2h..", "--app=loom")
	if c == a {
		t.Errorf("different range gave the same slug %q", a)
	}
	var n int64
	db.DB().Raw("SELECT count(*) FROM shared_links WHERE org_id = ?", org.ID).Scan(&n)
	if n != 2 {
		t.Errorf("rows = %d, want 2 (a repeat share must not add a row)", n)
	}
}

func TestSharedLink_ResolvesOnlyInItsOwnOrg(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("sl-iso-a")
	other, _ := db.FindOrCreateOrg("sl-iso-b")
	user, _ := db.CreateUser("alice-sl2@example.test")

	slug, _ := db.SaveSharedLink(org.ID, user.ID, "/bin/scan", "1h..", "x y")
	got, err := db.GetSharedLink(org.ID, slug)
	if err != nil || got.Bin != "/bin/scan" || got.RangeSpec != "1h.." || got.Args != "x y" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := db.GetSharedLink(other.ID, slug); err == nil {
		t.Error("another org resolved the link")
	}
	if _, err := db.GetSharedLink(org.ID, "nosuchslug"); err == nil {
		t.Error("unknown slug resolved")
	}
	// The same payload in another org is a different link.
	if s2, _ := db.SaveSharedLink(other.ID, user.ID, "/bin/scan", "1h..", "x y"); s2 == slug {
		t.Error("slug must be org-scoped")
	}
}
