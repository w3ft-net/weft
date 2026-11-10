package store

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
)

func teamPermsStore(t *testing.T) *Store {
	t.Helper()
	dsn := TestDSN(t)
	db, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() {
		db.TruncateTestTables()
		db.Close()
	})
	return db
}

func addTeamUser(t *testing.T, db *Store, custID OrgID, email, role string) *User {
	t.Helper()
	u, err := db.CreateUser(email)
	if err != nil {
		t.Fatal(err)
	}
	r := orgs.RoleMember
	if role == "owner" {
		r = RoleOwner
	}
	err = db.AddTeamMemberWithEmail(custID, u.ID, u.Email, r)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestTeamPerms_BuildAndApply_RoundTrip(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tp-rt-%d", os.Getpid()))
	_ = addTeamUser(t, db, cust.ID, fmt.Sprintf("owner-%d@x.test", os.Getpid()), "owner")
	alice := addTeamUser(t, db, cust.ID, fmt.Sprintf("alice-%d@x.test", os.Getpid()), "member")
	bob := addTeamUser(t, db, cust.ID, fmt.Sprintf("bob-%d@x.test", os.Getpid()), "member")
	apiNs, _ := db.FindOrCreateNamespace(cust.ID, "api")
	webNs, _ := db.FindOrCreateNamespace(cust.ID, "web")

	apply := TeamPermsDoc{
		alice.Email: {
			Bin: AccessEdit, Cron: AccessReadOnly, Etc: AccessReadOnly,
			Namespaces: map[string]NamespaceAcl{
				"api": {Read: true, Write: true},
				"web": {Read: true},
			},
		},
		bob.Email: {
			Bin: AccessReadOnly, Cron: AccessReadOnly, Etc: AccessReadOnly,
			Namespaces: map[string]NamespaceAcl{
				"api": {Read: true},
			},
		},
	}
	if err := db.ApplyTeamPerms(cust.ID, apply); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Verify the store reflects the applied doc.
	op, err := db.GetOrgPermission(cust.ID, alice.ID)
	if err != nil || !op.CanManageFunctions || op.CanManageWatches || op.CanManageEtc {
		t.Fatalf("alice org perms: %+v (err=%v)", op, err)
	}
	if _, err := db.GetNamespacePermission(alice.ID, apiNs.ID); err != nil {
		t.Fatalf("alice/api perm: %v", err)
	}
	if _, err := db.GetNamespacePermission(alice.ID, webNs.ID); err != nil {
		t.Fatalf("alice/web perm: %v", err)
	}

	// BuildTeamPerms should reproduce the input doc (owners excluded).
	roundtrip, err := db.BuildTeamPerms(cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := roundtrip[alice.Email]; !ok {
		t.Errorf("alice missing from rebuilt doc: %+v", roundtrip)
	}
	if got := roundtrip[alice.Email].Bin; got != AccessEdit {
		t.Errorf("alice.Bin: got %q want %q", got, AccessEdit)
	}
	if got := roundtrip[bob.Email].Namespaces["api"].Read; !got {
		t.Errorf("bob/api.Read should be true")
	}
	// The owner appears in the synthesized doc. They used to be omitted,
	// because their access was implicit and there was nothing to show;
	// since migration 072 they hold real rows, and omitting them would
	// make the doc a partial view of the state it claims to be
	// authoritative over.
	//
	// Their bits here are read-only, not the all-edit their promotion
	// materialized, and that is the point: the `apply` above didn't list
	// them, and missing-from-doc means default-deny for owners too.
	ownerPerms, ok := roundtrip[fmt.Sprintf("owner-%d@x.test", os.Getpid())]
	if !ok {
		t.Fatalf("owner missing from synthesized doc: %+v", roundtrip)
	}
	if ownerPerms.Bin != AccessReadOnly || ownerPerms.Cron != AccessReadOnly || ownerPerms.Etc != AccessReadOnly {
		t.Errorf("owner omitted from the applied doc should be default-denied; got %+v", ownerPerms)
	}
}

func TestTeamPerms_Apply_DefaultDenyWhenAbsent(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tp-dd-%d", os.Getpid()))
	alice := addTeamUser(t, db, cust.ID, fmt.Sprintf("a-%d@x.test", os.Getpid()), "member")
	apiNs, _ := db.FindOrCreateNamespace(cust.ID, "api")

	// First apply: alice gets edit/api perms.
	first := TeamPermsDoc{
		alice.Email: {
			Bin: AccessEdit, Cron: AccessEdit, Etc: AccessEdit,
			Namespaces: map[string]NamespaceAcl{"api": {Read: true}},
		},
	}
	if err := db.ApplyTeamPerms(cust.ID, first); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if op, err := db.GetOrgPermission(cust.ID, alice.ID); err != nil || !op.CanManageFunctions {
		t.Fatalf("expected alice's perms after first apply")
	}

	// Second apply: alice is absent from the doc → all her perms wipe.
	if err := db.ApplyTeamPerms(cust.ID, TeamPermsDoc{}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if _, err := db.GetOrgPermission(cust.ID, alice.ID); err == nil {
		t.Error("alice's org perm row should have been deleted")
	}
	if _, err := db.GetNamespacePermission(alice.ID, apiNs.ID); err == nil {
		t.Error("alice's namespace perm row should have been deleted")
	}
}

func TestTeamPerms_Apply_RejectsUnknownMember(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tp-bad-%d", os.Getpid()))
	doc := TeamPermsDoc{
		"ghost@example.com": {Bin: AccessEdit},
	}
	err := db.ApplyTeamPerms(cust.ID, doc)
	if err == nil || !strings.Contains(err.Error(), "unknown team member") {
		t.Fatalf("expected unknown-member error, got: %v", err)
	}
}

// TestTeamPerms_Apply_AcceptsOwner: the doc governs an owner's bits like
// anyone else's, downgrades included. Listing an owner used to be
// rejected outright ("owners have implicit full access"); that fork is
// gone along with the implicit access it described.
//
// Note what this means and doesn't: stripping an owner's bits does not
// strip their role, so they keep the right to edit the doc and can grant
// themselves back.
func TestTeamPerms_Apply_AcceptsOwner(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tp-own-%d", os.Getpid()))
	owner := addTeamUser(t, db, cust.ID, fmt.Sprintf("o-%d@x.test", os.Getpid()), "owner")
	doc := TeamPermsDoc{
		owner.Email: {Bin: AccessReadOnly, Cron: AccessEdit, Etc: AccessReadOnly},
	}
	if err := db.ApplyTeamPerms(cust.ID, doc); err != nil {
		t.Fatalf("ApplyTeamPerms with an owner listed: %v", err)
	}

	perm, err := db.GetOrgPermission(cust.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if perm.CanManageFunctions || perm.CanManageEtc {
		t.Errorf("owner's bin/etc should be downgraded to read-only; got %+v", perm)
	}
	if !perm.CanManageWatches {
		t.Errorf("owner's cron should stay edit; got %+v", perm)
	}
}

func TestTeamPerms_Apply_RejectsUnknownNamespace(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tp-ns-%d", os.Getpid()))
	alice := addTeamUser(t, db, cust.ID, fmt.Sprintf("a-%d@x.test", os.Getpid()), "member")
	doc := TeamPermsDoc{
		alice.Email: {
			Bin: AccessReadOnly, Cron: AccessReadOnly, Etc: AccessReadOnly,
			Namespaces: map[string]NamespaceAcl{"nope": {Read: true}},
		},
	}
	err := db.ApplyTeamPerms(cust.ID, doc)
	if err == nil || !strings.Contains(err.Error(), "unknown namespace") {
		t.Fatalf("expected unknown-namespace error, got: %v", err)
	}
}
