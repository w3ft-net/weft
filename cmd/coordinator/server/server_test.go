package server

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/notice"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/tenancy"
)

// A coordinator with no way to authenticate its callers must not start, so the
// zero adapter is refused with a message that says why.
func TestAdapter_RequiresAnIdentity(t *testing.T) {
	err := Adapter[*testDB]{}.validate()
	if err == nil || !strings.Contains(err.Error(), "Identity") {
		t.Fatalf("validate() = %v, want an error naming Identity", err)
	}
	if err := withContracts(Adapter[*testDB]{Identity: noIdentity}).validate(); err != nil {
		t.Errorf("an adapter with an Identity was refused: %v", err)
	}
}

// The coordinator has no default for where its data lives: an adapter that
// leaves a contract unset is refused, naming it.
func TestAdapter_RequiresEachContract(t *testing.T) {
	a := withContracts(Adapter[*testDB]{Identity: noIdentity})
	a.Crons = nil
	if err := a.validate(); err == nil || !strings.Contains(err.Error(), "Crons") {
		t.Fatalf("validate() = %v, want an error naming Crons", err)
	}
}

// withContracts is a with every contract the coordinator requires set to one
// that holds nothing.
func withContracts(a Adapter[*testDB]) Adapter[*testDB] {
	a.Tokens = func(*testDB) token.Store { return nil }
	a.Sources = func(*testDB) source.Store { return nil }
	a.Notices = func(*testDB) notice.Store { return nil }
	a.Crons = func(*testDB) cron.Store { return nil }
	a.Bins = func(*testDB) bin.Store { return nil }
	a.Notifier = func(*testDB) *coordinator.Notifier { return nil }
	a.Coordinator.Mounts = func(*testDB) coordinator.MountsFunc { return nil }
	a.Coordinator.Orgs = defaults.OpenOrgs{}
	a.Coordinator.Scoper = func(*testDB) tenancy.Scoper { return defaults.OpenScoper{} }
	a.Coordinator.Authorizer = func(*testDB) tenancy.Authorizer { return defaults.OpenAuthorizer{} }
	a.Open = func(string) (*testDB, func(), error) { return nil, func() {}, nil }
	a.Coordination = func(*testDB) coordination.Coordination { return nil }
	a.Shards = func(*testDB) logdb.ShardStore { return nil }
	a.Services = func(*testDB) logdb.ServiceDirectory { return nil }
	a.IngestFailures = func(*testDB) ingestfail.Store { return nil }
	return a
}

// The open adapter is a usable adapter once its contracts are set.
func TestAdapter_OpenAdapterIsUsable(t *testing.T) {
	a := withContracts(Adapter[*testDB]{Identity: noIdentity})
	if err := a.validate(); err != nil {
		t.Fatal(err)
	}
	if got := a.retention(nil, "us-east-2"); got != nil {
		t.Errorf("an adapter without Retention built one: %v", got)
	}
}

func noIdentity(*testDB) identity.Server { return nil }

// testDB stands for whatever database a deployment opens.
type testDB struct{}
