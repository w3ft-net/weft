package store

import (
	"testing"
)

// TestSeedDevSubscription_MultipleCustomers reproduces the bug where
// SeedDevSubscription hardcodes the row's primary key as "dev-sub" —
// so the second customer's call hits a uniqueness violation against
// the first customer's row, silently fails (the function swallows
// the Create error), and aborts the surrounding transaction.
//
// Symptom in the wild: in tests that use BeginTest, the next DB call
// after SeedDevSubscription returns SQLSTATE 25P02 ("current
// transaction is aborted"), even though SeedDevSubscription itself
// returns void.
//
// Expected behaviour: each customer ends up with its own active dev
// subscription, and the surrounding transaction stays healthy.
func TestSeedDevSubscription_MultipleCustomers(t *testing.T) {
	db := testOpen(t)

	alpha, err := db.FindOrCreateCustomer("seed-mc-alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := db.FindOrCreateCustomer("seed-mc-beta")
	if err != nil {
		t.Fatal(err)
	}

	db.SeedDevSubscription(alpha.Name)
	db.SeedDevSubscription(beta.Name)

	subA, err := db.ActiveSubscription(alpha.Name)
	if err != nil {
		t.Fatalf("alpha ActiveSubscription: %v", err)
	}
	if subA == nil {
		t.Fatal("alpha has no active subscription after seed")
	}
	subB, err := db.ActiveSubscription(beta.Name)
	if err != nil {
		t.Fatalf("beta ActiveSubscription: %v", err)
	}
	if subB == nil {
		t.Fatal("beta has no active subscription after seed (PK collision likely)")
	}

	// The transaction must still be usable — a follow-up insert
	// would fail with SQLSTATE 25P02 if the silent Create error in
	// the second SeedDevSubscription poisoned it.
	if _, err := db.CreateUser("seed-mc-canary@example.test"); err != nil {
		t.Fatalf("post-seed CreateUser failed (transaction poisoned?): %v", err)
	}
}

// TestSeedDevSubscription_Idempotent confirms re-seeding the same
// customer is a no-op rather than producing duplicate rows or
// aborting on PK conflict against the customer's own prior row.
func TestSeedDevSubscription_Idempotent(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("seed-idem")
	if err != nil {
		t.Fatal(err)
	}

	db.SeedDevSubscription(cust.Name)
	db.SeedDevSubscription(cust.Name)
	db.SeedDevSubscription(cust.Name)

	// Each customer should have exactly one active dev subscription
	// regardless of seed-call count.
	var count int64
	if err := db.db.Model(&Subscription{}).
		Where("customer_id = ? AND status = 'active'", cust.Name).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 active subscription for %s, got %d", cust.Name, count)
	}
}
