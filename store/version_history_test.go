package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// Helpers ----------------------------------------------------------

// historyCount returns the number of history rows for a function.
// Used by trigger-behaviour tests that need to assert on raw counts
// rather than going through the Log API.
func historyCount(t *testing.T, db *Store, customerID uint, name string) int {
	t.Helper()
	var n int64
	err := db.db.Raw(`
		SELECT COUNT(*) FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.customer_id = ? AND f.name = ?
	`, customerID, name).Scan(&n).Error
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
	cust, _ := db.FindOrCreateCustomer("vh-first")
	user, _ := db.CreateUser("alice-first@example.test")

	if err := db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v1"); err != nil {
		t.Fatal(err)
	}
	if got := historyCount(t, db, cust.ID, "scan"); got != 0 {
		t.Errorf("first save should produce no history rows; got %d", got)
	}
}

func TestShellFunction_UpdateCapturesPriorVersion(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-update")
	alice, _ := db.CreateUser("alice-up@example.test")
	bob, _ := db.CreateUser("bob-up@example.test")

	// Alice writes v1.
	if err := db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v1"); err != nil {
		t.Fatal(err)
	}
	// Bob overwrites with v2 — captures v1 into history.
	if err := db.SaveShellFunction(cust.ID, bob.ID, "scan", "echo v2"); err != nil {
		t.Fatal(err)
	}

	if got := historyCount(t, db, cust.ID, "scan"); got != 1 {
		t.Fatalf("after overwrite: got %d history rows, want 1", got)
	}

	type row struct {
		Source              string
		ContentHash         string
		AuthoredByUserID    *string
		CapturedByUserID    *string
	}
	var r row
	err := db.db.Raw(`
		SELECT h.source, h.content_hash, h.authored_by_user_id, h.captured_by_user_id
		FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.customer_id = ? AND f.name = ?
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
	cust, _ := db.FindOrCreateCustomer("vh-chain")
	user, _ := db.CreateUser("alice-chain@example.test")

	for _, src := range []string{"echo v1", "echo v2", "echo v3", "echo v4"} {
		if err := db.SaveShellFunction(cust.ID, user.ID, "scan", src); err != nil {
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
		WHERE f.customer_id = ? AND f.name = ?
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
	cust, _ := db.FindOrCreateCustomer("vh-cap")
	user, _ := db.CreateUser("alice-cap@example.test")

	// 7 saves: 1 initial + 6 updates. After update 6, history has
	// captured v1 through v6. The cap drops the oldest (v1) when
	// the 6th update fires, leaving history at exactly 5 rows.
	for i := 1; i <= 7; i++ {
		src := "echo v" + string(rune('0'+i))
		if err := db.SaveShellFunction(cust.ID, user.ID, "scan", src); err != nil {
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
		WHERE f.customer_id = ? AND f.name = ?
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
	cust, _ := db.FindOrCreateCustomer("vh-noacting")
	alice, _ := db.CreateUser("alice-noact@example.test")

	if err := db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v1"); err != nil {
		t.Fatal(err)
	}
	// Update with empty userID — system call.
	if err := db.SaveShellFunction(cust.ID, "", "scan", "echo v2"); err != nil {
		t.Fatal(err)
	}

	var capturedBy *string
	err := db.db.Raw(`
		SELECT h.captured_by_user_id FROM shell_function_history h
		JOIN shell_functions f ON f.id = h.function_id
		WHERE f.customer_id = ? AND f.name = ?
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
	a, _ := db.FindOrCreateCustomer("vh-hash-a")
	b, _ := db.FindOrCreateCustomer("vh-hash-b")

	if err := db.SaveShellFunction(a.ID, "", "scan", "echo same"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(b.ID, "", "scan", "echo same"); err != nil {
		t.Fatal(err)
	}

	var hashA, hashB string
	db.db.Raw(`SELECT content_hash FROM shell_functions WHERE customer_id = ? AND name = ?`,
		a.ID, "scan").Row().Scan(&hashA)
	db.db.Raw(`SELECT content_hash FROM shell_functions WHERE customer_id = ? AND name = ?`,
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

func TestLogShellFunction_OrderAndShape(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-log")
	user, _ := db.CreateUser("alice-log@example.test")

	saves := []string{"echo v1", "echo v2", "echo v3"}
	for _, src := range saves {
		if err := db.SaveShellFunction(cust.ID, user.ID, "scan", src); err != nil {
			t.Fatal(err)
		}
	}

	versions, err := db.LogShellFunction(cust.ID, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("Log: got %d versions, want 3 (HEAD + 2 history)", len(versions))
	}
	// Newest first: v3 (HEAD), v2, v1.
	if versions[0].Source != "echo v3" || !versions[0].IsHead {
		t.Errorf("versions[0]: got source=%q IsHead=%v, want HEAD with v3",
			versions[0].Source, versions[0].IsHead)
	}
	if versions[1].Source != "echo v2" || versions[1].IsHead {
		t.Errorf("versions[1]: got source=%q IsHead=%v, want history v2",
			versions[1].Source, versions[1].IsHead)
	}
	if versions[2].Source != "echo v1" || versions[2].IsHead {
		t.Errorf("versions[2]: got source=%q IsHead=%v, want history v1",
			versions[2].Source, versions[2].IsHead)
	}
}

// Store API: ResolveRef -------------------------------------------

func TestResolveShellFunctionRef_HEAD(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-head")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo current")

	v, err := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "echo current" || !v.IsHead {
		t.Errorf("HEAD: got source=%q IsHead=%v, want current head row", v.Source, v.IsHead)
	}
}

func TestResolveShellFunctionRef_HEADCaret(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-caret")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo old")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo new")

	v, err := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD^")
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "echo old" {
		t.Errorf("HEAD^: got source=%q, want %q", v.Source, "echo old")
	}
	if v.IsHead {
		t.Errorf("HEAD^: IsHead should be false")
	}
}

func TestResolveShellFunctionRef_HEADCaretN(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-caretn")
	user, _ := db.CreateUser("alice@example.test")
	for _, s := range []string{"v1", "v2", "v3", "v4"} {
		db.SaveShellFunction(cust.ID, user.ID, "scan", s)
	}

	v, err := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD^2")
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "v2" {
		t.Errorf("HEAD^2: got source=%q, want v2", v.Source)
	}
}

func TestResolveShellFunctionRef_HashPrefix(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-hash")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo target")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo other")

	target := hashOf("echo target")
	prefix := target[:8]

	v, err := db.ResolveShellFunctionRef(cust.ID, "scan", prefix)
	if err != nil {
		t.Fatalf("hash prefix lookup: %v", err)
	}
	if v.Source != "echo target" {
		t.Errorf("by hash prefix: got source=%q, want target", v.Source)
	}
	if v.ContentHash != target {
		t.Errorf("ContentHash: got %q, want %q", v.ContentHash, target)
	}
}

func TestResolveShellFunctionRef_ShortPrefixRejected(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-short")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo target")

	// 3 chars is below the 4-char minimum.
	short := hashOf("echo target")[:3]
	if _, err := db.ResolveShellFunctionRef(cust.ID, "scan", short); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("short prefix: got %v, want ErrInvalidRef", err)
	}
}

func TestResolveShellFunctionRef_UnknownHash(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-unknown")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo here")

	if _, err := db.ResolveShellFunctionRef(cust.ID, "scan", "deadbeef"); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("unknown hash: got %v, want ErrRefNotFound", err)
	}
}

func TestResolveShellFunctionRef_BeyondCap(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-resolve-beyond")
	user, _ := db.CreateUser("alice@example.test")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "v1")

	// HEAD^1 with no history should fail.
	if _, err := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD^"); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("HEAD^ with no history: got %v, want ErrRefNotFound", err)
	}
	// HEAD^7 always fails (cap is 5).
	if _, err := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD^7"); err == nil {
		t.Errorf("HEAD^7 should error; got nil")
	}
}

// Store API: Checkout ---------------------------------------------

func TestCheckoutShellFunction_RoundTrip(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-checkout")
	alice, _ := db.CreateUser("alice@example.test")
	bob, _ := db.CreateUser("bob@example.test")

	// Alice writes v1, alice writes v2, bob writes v3.
	db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v1")
	db.SaveShellFunction(cust.ID, alice.ID, "scan", "echo v2")
	db.SaveShellFunction(cust.ID, bob.ID, "scan", "echo v3")

	// Bob checks out HEAD^2 (which is v1 by alice).
	if err := db.CheckoutShellFunction(cust.ID, bob.ID, "scan", "HEAD^2"); err != nil {
		t.Fatal(err)
	}

	// Live row's source matches v1.
	fn, err := db.GetShellFunction(cust.ID, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if fn.Source != "echo v1" {
		t.Errorf("after checkout: live source = %q, want echo v1", fn.Source)
	}
	// Original author of v1 was alice; checkout should preserve
	// authored_by_user_id on the new live row.
	if fn.CreatedByUserID == nil || *fn.CreatedByUserID != alice.ID {
		t.Errorf("checkout should preserve original author; got %v, want alice", fn.CreatedByUserID)
	}
}

// Store API: Amend ------------------------------------------------

func TestAmendShellFunctionMessage_HEAD_DoesNotCapture(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-amend-head")
	user, _ := db.CreateUser("alice@example.test")

	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v1")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "echo v2")
	before := historyCount(t, db, cust.ID, "scan")

	if err := db.AmendShellFunctionMessage(cust.ID, "scan", "HEAD", "first message"); err != nil {
		t.Fatal(err)
	}

	if got := historyCount(t, db, cust.ID, "scan"); got != before {
		t.Errorf("amend should NOT create a history row; before=%d after=%d", before, got)
	}

	// And the message persisted.
	v, _ := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD")
	if v.Message != "first message" {
		t.Errorf("after amend: HEAD message = %q, want %q", v.Message, "first message")
	}
}

func TestAmendShellFunctionMessage_HistoryRef(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("vh-amend-history")
	user, _ := db.CreateUser("alice@example.test")

	db.SaveShellFunction(cust.ID, user.ID, "scan", "v1")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "v2")
	db.SaveShellFunction(cust.ID, user.ID, "scan", "v3")

	if err := db.AmendShellFunctionMessage(cust.ID, "scan", "HEAD^2", "v1's late label"); err != nil {
		t.Fatal(err)
	}

	v, _ := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD^2")
	if v.Message != "v1's late label" {
		t.Errorf("HEAD^2 after amend: message = %q, want %q", v.Message, "v1's late label")
	}
	// HEAD and HEAD^ messages unchanged.
	head, _ := db.ResolveShellFunctionRef(cust.ID, "scan", "HEAD")
	if head.Message != "" {
		t.Errorf("HEAD message changed unexpectedly: %q", head.Message)
	}
}
