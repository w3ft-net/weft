package defaults

import (
	"database/sql"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/login"
	"github.com/w3ft-net/weft/backends/login/loginstoretest"
	"github.com/w3ft-net/weft/sqlitedb"
)

func TestLoginConformance(t *testing.T) {
	loginstoretest.Run(t, func(t *testing.T) loginstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewLoginBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return loginstoretest.Subject{
			Store: s,
			Allow: func(t *testing.T, email string) { allowLogin(t, db, email) },
		}
	})
}

func allowLogin(t *testing.T, db *sql.DB, email string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO login_emails (email, created_at) VALUES (?, ?)`,
		login.Normalize(email), time.Now().UTC().Format(loginTimeFmt)); err != nil {
		t.Fatal(err)
	}
}

// A copy of the database cannot sign anyone in: it holds the link's hash, not the
// token the email carried.
func TestLoginKeepsOnlyTheHashOfALink(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := NewLoginBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueLink("ada@example.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM login_links`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw || stored != login.Hash(raw) {
		t.Errorf("stored %q for raw %q; want its sha256", stored, raw)
	}
}

// Links that can no longer be used are not kept for ever.
func TestLoginIssuePrunesDeadLinks(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := NewLoginBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueLink("ada@example.test", -time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueLink("ada@example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM login_links`).Scan(&n); err != nil || n != 1 {
		t.Errorf("links held = %d (err %v); want only the live one", n, err)
	}
}

func TestLoginIsInTheOpenStores(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if st.Login == nil {
		t.Fatal("Open made no login store")
	}
}
