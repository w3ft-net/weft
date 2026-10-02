//go:build test

package store

import "testing"

// testOrgIDs gives the well-known test customers fixed ids. Fixtures that are
// built before the database exists (shard rows, parquet) carry customer_id as
// a constant, so the org they belong to has to be created with that same id.
var testOrgIDs = map[string]OrgID{
	"kevlar": 1, "vimgym": 2, "victim-co": 3, "tenant-a": 4, "acme": 5,
	"globex": 6, "testcust": 7, "paying-co": 8, "graybeard": 9, "c": 10,
	"x": 11, "victim": 12, "unmapped-co": 13, "ghost": 14,
}

// TestOrg returns the org with this well-known test name, creating it with its
// fixed id if it does not exist yet. The identity sequence is first moved past
// every fixed id (sequences are not transactional, so this survives a rolled
// back test transaction), so orgs created the normal way never collide.
func TestOrg(t testing.TB, db *Store, name string) *Organization {
	t.Helper()
	id, ok := testOrgIDs[name]
	if !ok {
		t.Fatalf("TestOrg: %q has no fixed test id; add it to testOrgIDs", name)
	}
	if existing, err := db.GetOrgByName(name); err == nil {
		return existing
	}
	if err := db.db.Exec(`SELECT setval(pg_get_serial_sequence('organizations', 'id'),
		GREATEST((SELECT last_value FROM organizations_id_seq), 1000))`).Error; err != nil {
		t.Fatalf("TestOrg: advance org id sequence: %v", err)
	}
	org := &Organization{ID: id, Name: name}
	if err := db.db.Create(org).Error; err != nil {
		t.Fatalf("TestOrg(%q): %v", name, err)
	}
	return org
}
