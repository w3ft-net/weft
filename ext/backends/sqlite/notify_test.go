package sqlite

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/notify/notifystoretest"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
)

func TestNotifyConformance(t *testing.T) {
	notifystoretest.Run(t, func(t *testing.T) notifystoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewNotifyBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return notifystoretest.Subject{Aliases: s, Secrets: s, User: "u1", OtherUser: "u2"}
	})
}

func TestNotifyConformance_Encrypted(t *testing.T) {
	t.Setenv("WEFT_SECRETS_KEY", "conformance-key")
	TestNotifyConformance(t)
}

func TestNotifySecrets_EncryptedAtRest(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	t.Setenv("WEFT_SECRETS_KEY", "key-one")
	s, err := NewNotifyBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(shape.Added{}, "slack_token", "xoxb-secret"); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT value FROM notify_secrets WHERE name = 'slack_token'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "xoxb-secret") || !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("stored value %q is not ciphertext", stored)
	}
	if got, err := s.Value(shape.Added{}, "slack_token"); err != nil || got != "xoxb-secret" {
		t.Fatalf("Value = %q, %v", got, err)
	}

	// A ciphertext copied under another name does not open.
	db.Exec(`INSERT INTO notify_secrets (name, value) VALUES ('other', ?)`, stored)
	if _, err := s.Value(shape.Added{}, "other"); err == nil {
		t.Fatal("a ciphertext opened under a name it was not written for")
	}

	// Another key, and no key, refuse it; a value written in clear still reads.
	t.Setenv("WEFT_SECRETS_KEY", "key-two")
	wrong, _ := NewNotifyBackend(db)
	if _, err := wrong.Value(shape.Added{}, "slack_token"); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	t.Setenv("WEFT_SECRETS_KEY", "")
	clear, _ := NewNotifyBackend(db)
	if _, err := clear.Value(shape.Added{}, "slack_token"); err == nil {
		t.Fatal("read a ciphertext without a key")
	}
	clear.Set(shape.Added{}, "plain", "v")
	if got, _ := clear.Value(shape.Added{}, "plain"); got != "v" {
		t.Fatalf("plain secret = %q", got)
	}
}
