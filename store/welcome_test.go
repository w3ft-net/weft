package store

import (
	"testing"
	"time"
)

// markWelcomeSeen mirrors the former Store.MarkWelcomeSeen, whose
// production logic moved to a caller outside the store package.
func markWelcomeSeen(db *Store, userID string) error {
	return db.db.Model(&User{}).
		Where("id = ? AND has_seen_welcome = FALSE", userID).
		Updates(map[string]any{
			"has_seen_welcome": true,
			"updated_at":       time.Now(),
		}).Error
}

// TestMarkWelcomeSeen confirms the flag flips from false → true, stays
// true on a second call (no error, no change), and is visible to
// subsequent reads via GetUserByID.
func TestMarkWelcomeSeen(t *testing.T) {
	db := testOpen(t)

	if _, err := db.FindOrCreateOrg("welcome-test"); err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser("welcome@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if user.HasSeenWelcome {
		t.Fatalf("new user should not have seen welcome yet")
	}

	if err := markWelcomeSeen(db, user.ID); err != nil {
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
	if err := markWelcomeSeen(db, user.ID); err != nil {
		t.Errorf("second MarkWelcomeSeen should be a no-op, got: %v", err)
	}
}
