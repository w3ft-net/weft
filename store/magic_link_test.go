package store

import (
	"errors"
	"sync"
	"testing"
)

// TestMagicLink_HashedAtRest pins the post-audit invariant: the raw
// token we email the user must NOT be present in the magic_links row.
// If a future refactor quietly reverts to storing plaintext, this
// fails loudly.
func TestMagicLink_HashedAtRest(t *testing.T) {
	db := testOpen(t)

	raw, err := db.CreateMagicLink("hashed@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("empty raw token")
	}

	var stored string
	if err := db.db.Raw("SELECT token FROM magic_links WHERE email = ?", "hashed@example.com").Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored == raw {
		t.Fatalf("raw token stored in db plaintext: %q", stored)
	}
	if stored != hashMagicLinkToken(raw) {
		t.Fatalf("stored token is not the sha256 of raw: stored=%q want=%q", stored, hashMagicLinkToken(raw))
	}
}

// TestMagicLink_ConcurrentConsume would have caught the pre-audit
// Validate+Consume TOCTOU: two concurrent Consume calls with the same
// token must produce exactly one success. The atomic UPDATE in
// ConsumeMagicLink is what guarantees that — the DB serializes the
// row update and the loser sees RowsAffected=0.
func TestMagicLink_ConcurrentConsume(t *testing.T) {
	// Needs a fresh connection pool (not a BeginTest transaction) —
	// pgx serializes queries on one connection and two goroutines racing
	// on one tx will trip the driver instead of the DB's row lock.
	// Same dedicated `weft_test` database as testOpen so this race test
	// doesn't write into the dev/integration stack's `weft` DB.
	dsn := TestDSN(t)
	db, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer db.Close()

	email := "race@example.com"
	defer db.db.Exec("DELETE FROM magic_links WHERE email = ?", email)

	raw, err := db.CreateMagicLink(email)
	if err != nil {
		t.Fatal(err)
	}

	const N = 20
	var wg sync.WaitGroup
	results := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_, results[i] = db.ConsumeMagicLink(raw)
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, e := range results {
		if e == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 winner in race, got %d", successes)
	}
}

// TestMagicLink_ConsumeReturnsEmail sanity-checks the happy path: a
// freshly-created token, consumed once, returns the email it was
// created for.
func TestMagicLink_ConsumeReturnsEmail(t *testing.T) {
	db := testOpen(t)

	raw, err := db.CreateMagicLink("happy@example.com")
	if err != nil {
		t.Fatal(err)
	}

	link, err := db.ConsumeMagicLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if link.Email != "happy@example.com" || link.Purpose != MagicLinkLogin {
		t.Errorf("expected a login link for happy@example.com, got %q / %q", link.Email, link.Purpose)
	}

	// Second consume of the same token fails.
	if _, err := db.ConsumeMagicLink(raw); err == nil {
		t.Error("second consume should have failed")
	}
}

// A link keeps what it was issued for: the purpose and the pending signup or
// invite it carries come back out of Consume, and the address is folded to
// the one spelling the store keeps.
func TestMagicLink_CarriesPurposeAndPayload(t *testing.T) {
	db := testOpen(t)

	raw, err := db.IssueMagicLink(&MagicLink{
		Email: "  Carry@Example.COM ", Purpose: MagicLinkSignup, OrgName: "acme", Tier: "swatch",
	})
	if err != nil {
		t.Fatal(err)
	}
	link, err := db.ConsumeMagicLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if link.Email != "carry@example.com" || link.Purpose != MagicLinkSignup || link.OrgName != "acme" || link.Tier != "swatch" {
		t.Errorf("link did not round-trip: %+v", link)
	}
}

// A redemption that fails after consuming the link must put it back: InTx
// rolls the consumption back with everything else.
func TestMagicLink_ConsumeRollsBackWithTransaction(t *testing.T) {
	db := testOpen(t)
	raw, err := db.CreateMagicLink("rollback@example.com")
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err = db.InTx(func(tx *Store) error {
		if _, err := tx.ConsumeMagicLink(raw); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v, want %v", err, boom)
	}
	if _, err := db.PeekMagicLink(raw); err != nil {
		t.Errorf("link stayed consumed after the transaction rolled back: %v", err)
	}
}

func TestCreateOrg_RefusesExistingName(t *testing.T) {
	db := testOpen(t)
	if _, err := db.CreateOrg("createorg-once"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrg("createorg-once"); !errors.Is(err, ErrOrgNameTaken) {
		t.Errorf("second CreateOrg = %v, want ErrOrgNameTaken", err)
	}
}
