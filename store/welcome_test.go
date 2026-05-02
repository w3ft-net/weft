package store

import "testing"

// TestMarkWelcomeSeen confirms the flag flips from false → true, stays
// true on a second call (no error, no change), and is visible to
// subsequent reads via GetUserByID.
func TestMarkWelcomeSeen(t *testing.T) {
	db := testOpen(t)

	if _, err := db.FindOrCreateCustomer("welcome-test"); err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser("welcome@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if user.HasSeenWelcome {
		t.Fatalf("new user should not have seen welcome yet")
	}

	if err := db.MarkWelcomeSeen(user.ID); err != nil {
		t.Fatalf("first MarkWelcomeSeen: %v", err)
	}

	got, err := db.GetUserByID(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasSeenWelcome {
		t.Errorf("expected HasSeenWelcome=true after MarkWelcomeSeen")
	}

	// Idempotent: the second call is a no-op (WHERE clause filters on
	// has_seen_welcome = FALSE), not an error.
	if err := db.MarkWelcomeSeen(user.ID); err != nil {
		t.Errorf("second MarkWelcomeSeen should be a no-op, got: %v", err)
	}
}
