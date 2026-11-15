package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/backends/notice/noticestoretest"
	"github.com/w3ft-net/weft/sqlitedb"
)

func TestNoticeConformance(t *testing.T) {
	noticestoretest.Run(t, func(t *testing.T) noticestoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewNoticeBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return noticestoretest.Subject{Store: s, User: "u1", Other: "u2"}
	})
}
