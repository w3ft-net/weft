package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/backends/dashboard/dashboardstoretest"
	"github.com/w3ft-net/weft/sqlitedb"
)

func TestDashboardConformance(t *testing.T) {
	dashboardstoretest.Run(t, func(t *testing.T) dashboardstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewDashboardBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return dashboardstoretest.Subject{Store: s}
	})
}

func TestDashboardIsInTheOpenStores(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if st.Dashboards == nil {
		t.Fatal("Open made no dashboard store")
	}
}
