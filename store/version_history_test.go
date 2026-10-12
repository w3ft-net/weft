package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Helpers ----------------------------------------------------------

// historyCount returns the number of history rows for a function.
// Used by trigger-behaviour tests that need to assert on raw counts
// rather than going through the Log API.
func historyCount(t *testing.T, db *Store, orgID OrgID, name string) int {
	t.Helper()
	var n int64
	err := db.db.Raw(`
		SELECT COUNT(*) FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.org_id = ? AND f.name = ?
	`, orgID, name).Scan(&n).Error
	if err != nil {
		t.Fatalf("historyCount: %v", err)
	}
	return int(n)
}

// hashOf is the canonical content hash — SHA-256 of source bytes,
// hex-encoded. Tests use this to assert determinism and to construct
// ref-by-hash queries.
func hashOf(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])
}

// SQL / trigger layer ---------------------------------------------

func TestShellFunction_FirstSaveCreatesNoHistory(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-first")
	user, _ := db.CreateUser("alice-first@example.test")

	if err := db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v1", "bash"); err != nil {
		t.Fatal(err)
	}
	if got := historyCount(t, db, cust.ID, "scan"); got != 0 {
		t.Errorf("first save should produce no history rows; got %d", got)
	}
}

func TestShellFunction_IdenticalResaveDoesNotCapture(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-noop")
	user, _ := db.CreateUser("alice-noop@example.test")

	// First save creates HEAD, no history (covered above).
	if err := db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v1", "bash"); err != nil {
		t.Fatal(err)
	}
	// Re-saving identical content (e.g. :w on an unchanged buffer)
	// must not append a duplicate-hash history row, otherwise HEAD^N
	// navigation and hash-prefix resolution get clogged with no-op
	// artifacts.
	for i := 0; i < 3; i++ {
		if err := db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v1", "bash"); err != nil {
			t.Fatal(err)
		}
	}
	if got := historyCount(t, db, cust.ID, "scan"); got != 0 {
		t.Errorf("identical re-saves should produce no history rows; got %d", got)
	}

	// And a real change after the no-ops captures exactly one row.
	if err := db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v2", "bash"); err != nil {
		t.Fatal(err)
	}
	if got := historyCount(t, db, cust.ID, "scan"); got != 1 {
		t.Errorf("real change after no-ops: got %d rows, want 1", got)
	}
}

func TestShellFunction_UpdateCapturesPriorVersion(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-update")
	alice, _ := db.CreateUser("alice-up@example.test")
	bob, _ := db.CreateUser("bob-up@example.test")

	// Alice writes v1.
	if err := db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v1", "bash"); err != nil {
		t.Fatal(err)
	}
	// Bob overwrites with v2 — captures v1 into history.
	if err := db.SaveShellFunction(cust.ID, bob.ID, "scan", "echo v2", "bash"); err != nil {
		t.Fatal(err)
	}

	if got := historyCount(t, db, cust.ID, "scan"); got != 1 {
		t.Fatalf("after overwrite: got %d history rows, want 1", got)
	}

	type row struct {
		Source           string
		ContentHash      string
		AuthoredByUserID *string
		CapturedByUserID *string
	}
	var r row
	err := db.db.Raw(`
		SELECT h.source, h.content_hash, h.authored_by_user_id, h.captured_by_user_id
		FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.org_id = ? AND f.name = ?
	`, cust.ID, "scan").Scan(&r).Error
	if err != nil {
		t.Fatal(err)
	}

	if r.Source != "echo v1" {
		t.Errorf("captured source: got %q, want %q", r.Source, "echo v1")
	}
	if r.ContentHash != hashOf("echo v1") {
		t.Errorf("captured content_hash: got %q, want %q", r.ContentHash, hashOf("echo v1"))
	}
	if r.AuthoredByUserID == nil || *r.AuthoredByUserID != alice.ID {
		t.Errorf("authored_by: got %v, want alice's ID %q", r.AuthoredByUserID, alice.ID)
	}
	if r.CapturedByUserID == nil || *r.CapturedByUserID != bob.ID {
		t.Errorf("captured_by: got %v, want bob's ID %q", r.CapturedByUserID, bob.ID)
	}
}

func TestShellFunction_PredecessorChain(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-chain")
	user, _ := db.CreateUser("alice-chain@example.test")

	for _, src := range []string{"echo v1", "echo v2", "echo v3", "echo v4"} {
		if err := db.SaveShellFunction(cust.ID, user.ID, "scan", src, "bash"); err != nil {
			t.Fatal(err)
		}
	}

	// Three history rows: v1, v2, v3 (v4 is HEAD, not in history).
	type row struct {
		ID            int64
		Source        string
		PredecessorID *int64
	}
	var rows []row
	err := db.db.Raw(`
		SELECT h.id, h.source, h.predecessor_id
		FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.org_id = ? AND f.name = ?
		ORDER BY h.captured_at ASC, h.id ASC
	`, cust.ID, "scan").Scan(&rows).Error
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 history rows, got %d", len(rows))
	}
	// Oldest first: v1 has no predecessor; v2 points at v1's ID; v3 points at v2's ID.
	if rows[0].Source != "echo v1" || rows[0].PredecessorID != nil {
		t.Errorf("v1 row: got source=%q pred=%v, want source=echo v1 pred=nil",
			rows[0].Source, rows[0].PredecessorID)
	}
	if rows[1].Source != "echo v2" || rows[1].PredecessorID == nil || *rows[1].PredecessorID != rows[0].ID {
		t.Errorf("v2 row: got source=%q pred=%v, want pred → v1 (%d)",
			rows[1].Source, rows[1].PredecessorID, rows[0].ID)
	}
	if rows[2].Source != "echo v3" || rows[2].PredecessorID == nil || *rows[2].PredecessorID != rows[1].ID {
		t.Errorf("v3 row: got source=%q pred=%v, want pred → v2 (%d)",
			rows[2].Source, rows[2].PredecessorID, rows[1].ID)
	}
}

func TestShellFunction_FiveCapDropsOldest(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-cap")
	user, _ := db.CreateUser("alice-cap@example.test")

	// 7 saves: 1 initial + 6 updates. After update 6, history has
	// captured v1 through v6. The cap drops the oldest (v1) when
	// the 6th update fires, leaving history at exactly 5 rows.
	for i := 1; i <= 7; i++ {
		src := "echo v" + string(rune('0'+i))
		if err := db.SaveShellFunction(cust.ID, user.ID, "scan", src, "bash"); err != nil {
			t.Fatal(err)
		}
	}

	if got := historyCount(t, db, cust.ID, "scan"); got != 5 {
		t.Fatalf("after 6 updates: got %d history rows, want 5 (cap)", got)
	}

	// The oldest history row should be v2 (v1 was dropped).
	var oldest string
	err := db.db.Raw(`
		SELECT h.source FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.org_id = ? AND f.name = ?
		ORDER BY h.captured_at ASC, h.id ASC LIMIT 1
	`, cust.ID, "scan").Scan(&oldest).Error
	if err != nil {
		t.Fatal(err)
	}
	if oldest != "echo v2" {
		t.Errorf("oldest history row: got %q, want %q (v1 should have been dropped)",
			oldest, "echo v2")
	}
}

func TestShellFunction_NoActingUserStoresNullCapturedBy(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-noacting")
	alice, _ := db.CreateUser("alice-noact@example.test")

	if err := db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v1", "bash"); err != nil {
		t.Fatal(err)
	}
	// Update with empty userID — system call.
	if err := db.SaveShellFunction(cust.ID, "", "scan", "echo v2", "bash"); err != nil {
		t.Fatal(err)
	}

	var capturedBy *string
	err := db.db.Raw(`
		SELECT h.captured_by_user_id FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.org_id = ? AND f.name = ?
	`, cust.ID, "scan").Row().Scan(&capturedBy)
	if err != nil {
		t.Fatal(err)
	}
	if capturedBy != nil {
		t.Errorf("system call should leave captured_by_user_id NULL; got %q", *capturedBy)
	}
}

// Store API: hashes -----------------------------------------------

func TestShellFunction_ContentHashIsDeterministic(t *testing.T) {
	db := testOpen(t)
	a, _ := db.FindOrCreateOrg("vh-hash-a")
	b, _ := db.FindOrCreateOrg("vh-hash-b")

	if err := db.SaveShellFunction(a.ID, "", "scan", "echo same", "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(b.ID, "", "scan", "echo same", "bash"); err != nil {
		t.Fatal(err)
	}

	var hashA, hashB string
	db.db.Raw(`SELECT content_hash FROM shell_functions WHERE org_id = ? AND name = ?`,
		a.ID, "scan").Row().Scan(&hashA)
	db.db.Raw(`SELECT content_hash FROM shell_functions WHERE org_id = ? AND name = ?`,
		b.ID, "scan").Row().Scan(&hashB)

	if hashA == "" {
		t.Fatal("content_hash not populated on first save")
	}
	if hashA != hashB {
		t.Errorf("identical content produced different hashes: %q vs %q", hashA, hashB)
	}
	if hashA != hashOf("echo same") {
		t.Errorf("content_hash: got %q, want %q", hashA, hashOf("echo same"))
	}
}

// Store API: Log --------------------------------------------------

// Store API: ResolveRef -------------------------------------------

// Store API: Checkout ---------------------------------------------

// Store API: Amend ------------------------------------------------

func TestAmendShellFunctionMessage_HEAD_DoesNotCapture(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-amend-head")
	user, _ := db.CreateUser("alice@example.test")

	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v1", "bash")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v2", "bash")
	before := historyCount(t, db, cust.ID, "scan")

	if err := db.AmendShellFunctionMessage(cust.ID, user.ID, "scan", "first message"); err != nil {
		t.Fatal(err)
	}

	if got := historyCount(t, db, cust.ID, "scan"); got != before {
		t.Errorf("amend should NOT create a history row; before=%d after=%d", before, got)
	}

	// And the message persisted.
	v, _ := db.GetShellFunction(cust.ID, "scan")
	if v.Message != "first message" {
		t.Errorf("after amend: HEAD message = %q, want %q", v.Message, "first message")
	}
}

func TestAmendShellFunctionMessage_RefusesAnotherAuthorsVersion(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vh-amend-other")
	alice, _ := db.CreateUser("alice-amend-other@example.test")
	bob, _ := db.CreateUser("bob-amend-other@example.test")

	db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v1", "bash")
	// Bob's save lands after Alice's; Alice's popup then tries to amend.
	db.SaveShellFunction(cust.ID, bob.ID, "scan", "echo v2", "bash")

	if err := db.AmendShellFunctionMessage(cust.ID, alice.ID, "scan", "mine"); err == nil {
		t.Fatal("alice amended bob's version")
	}
	if err := db.AmendShellFunctionMessage(cust.ID, bob.ID, "scan", "bob's"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetShellFunction(cust.ID, "scan"); v.Message != "bob's" {
		t.Errorf("message = %q", v.Message)
	}
}
