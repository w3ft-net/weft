package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func teamPermsStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
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

func addTeamUser(t *testing.T, db *Store, custID uint, email, role string) *User {
	t.Helper()
	u, err := db.CreateUser(email)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = db.CreateTeamMember(&TeamMember{
		ID:         uuid.New().String(),
		CustomerID: custID,
		Email:      u.Email,
		UserID:     &u.ID,
		Role:       role,
		Status:     "active",
		InvitedAt:  now,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestTeamPerms_BuildAndApply_RoundTrip(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("tp-rt-%d", os.Getpid()))
	_ = addTeamUser(t, db, cust.ID, fmt.Sprintf("owner-%d@x.test", os.Getpid()), "owner")
	alice := addTeamUser(t, db, cust.ID, fmt.Sprintf("alice-%d@x.test", os.Getpid()), "member")
	bob := addTeamUser(t, db, cust.ID, fmt.Sprintf("bob-%d@x.test", os.Getpid()), "member")
	apiNs, _ := db.FindOrCreateNamespace(cust.ID, "api")
	webNs, _ := db.FindOrCreateNamespace(cust.ID, "web")

	apply := TeamPermsDoc{
		alice.Email: {
			Bin: AccessEdit, Cron: AccessReadOnly, Etc: AccessReadOnly,
			Namespaces: map[string]NamespaceAcl{
				"api": {Read: true, CreateTokens: true},
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
	if _, owner := roundtrip[fmt.Sprintf("owner-%d@x.test", os.Getpid())]; owner {
		t.Error("owner should not appear in synthesized doc")
	}
}

func TestTeamPerms_Apply_DefaultDenyWhenAbsent(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("tp-dd-%d", os.Getpid()))
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
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("tp-bad-%d", os.Getpid()))
	doc := TeamPermsDoc{
		"ghost@example.com": {Bin: AccessEdit},
	}
	err := db.ApplyTeamPerms(cust.ID, doc)
	if err == nil || !strings.Contains(err.Error(), "unknown team member") {
		t.Fatalf("expected unknown-member error, got: %v", err)
	}
}

func TestTeamPerms_Apply_RejectsOwner(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("tp-own-%d", os.Getpid()))
	owner := addTeamUser(t, db, cust.ID, fmt.Sprintf("o-%d@x.test", os.Getpid()), "owner")
	doc := TeamPermsDoc{
		owner.Email: {Bin: AccessReadOnly},
	}
	err := db.ApplyTeamPerms(cust.ID, doc)
	if err == nil || !strings.Contains(err.Error(), "implicit full access") {
		t.Fatalf("expected owner-rejection error, got: %v", err)
	}
}

func TestTeamPerms_Apply_RejectsUnknownNamespace(t *testing.T) {
	db := teamPermsStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("tp-ns-%d", os.Getpid()))
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

func TestTeamPerms_Marshal_RoundTripsViaParse(t *testing.T) {
	doc := TeamPermsDoc{
		"alice@x.test": {
			Bin: AccessEdit, Cron: AccessReadOnly, Etc: AccessReadOnly,
			Namespaces: map[string]NamespaceAcl{
				"api": {Read: true, CreateTokens: true},
				"web": {Read: true},
			},
		},
		"bob@x.test": {
			Bin: AccessReadOnly, Cron: AccessReadOnly, Etc: AccessReadOnly,
		},
	}
	body, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	round, err := ParseTeamPerms(string(body))
	if err != nil {
		t.Fatalf("parse: %v\n---\n%s", err, body)
	}
	if got := round["alice@x.test"].Namespaces["api"].CreateTokens; !got {
		t.Errorf("CreateTokens did not round-trip; rebuilt: %+v", round)
	}
	if got := round["bob@x.test"].Bin; got != AccessReadOnly {
		t.Errorf("bob.Bin round-trip: got %q", got)
	}
}

func TestTeamPerms_Parse_RejectsInvalidLevel(t *testing.T) {
	body := "alice@x.test:\n  bin: write\n  cron: read-only\n  etc: read-only\n"
	_, err := ParseTeamPerms(body)
	if err == nil || !strings.Contains(err.Error(), "invalid access level") {
		t.Fatalf("expected invalid-level error, got: %v", err)
	}
}
