package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/backends/notify/notifystoretest"
	"github.com/w3ft-net/weft/sqlitedb"
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
