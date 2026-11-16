package server

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/sqlitedb"
)

// A coordinator with no way to authenticate its callers must not start, so the
// zero Deps is refused with a message that says why.
func TestDeps_RequiresAnIdentity(t *testing.T) {
	err := (&Deps{}).Validate()
	if err == nil || !strings.Contains(err.Error(), "Identity.Authn") {
		t.Fatalf("Validate() = %v, want an error naming Identity.Authn", err)
	}
	if err := (*Deps)(nil).Validate(); err == nil {
		t.Error("Validate() on nil Deps = nil, want an error")
	}
	if err := testDeps(t).Validate(); err != nil {
		t.Errorf("Deps with every required part was refused: %v", err)
	}
}

// The coordinator has no default for where its data lives: Deps that leave a
// backend unset are refused, naming it.
func TestDeps_RequiresEachBackend(t *testing.T) {
	d := testDeps(t)
	d.Backends.Crons = nil
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "Backends.Crons") {
		t.Fatalf("Validate() = %v, want an error naming Backends.Crons", err)
	}
}

// testDeps is Deps with every required part set, over one in-memory database.
func testDeps(t *testing.T) *Deps {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := defaults.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return &Deps{
		Identity: Identity{
			Authn: noAuthn{},
			Orgs:  defaults.OpenOrgs{},
			Scope: defaults.OpenScoper{},
			Authz: defaults.OpenAuthorizer{},
		},
		Backends: Backends{
			Tokens:         st.Tokens,
			Sources:        st.Sources,
			Notices:        st.Notices,
			Crons:          st.Crons,
			Bins:           st.Bins,
			IngestFailures: st.Ingest,
			Coordination:   st.Coord,
			Shards:         st.Shards,
			Services:       st.Sources,
		},
		Notifier: coordinator.NewNotifier(st.Notify, alerting.EmailTarget{}),
		Mounts:   &mounts.Backends{},
	}
}

// noAuthn authenticates nobody: the tests never serve a call.
type noAuthn struct{ identity.Server }
