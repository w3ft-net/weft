package server

import (
	"context"
	"github.com/w3ft-net/weft/logdb"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/deployment"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/identity/open"
	"github.com/w3ft-net/weft/mounts"
)

// A coordinator with no way to authenticate its callers must not start, so an
// empty deployment is refused with a message that says why.
func TestValidate_RequiresAnIdentity(t *testing.T) {
	err := validate(testDeployment(t, func(d *deployment.Deployment, _ *Own) { d.Authn.GRPC = nil }))
	if err == nil || !strings.Contains(err.Error(), "Authn.GRPC") {
		t.Fatalf("validate() = %v, want an error naming Authn.GRPC", err)
	}
	if err := validate(nil, nil); err == nil {
		t.Error("validate(nil, nil) = nil, want an error")
	}
	if err := validate(testDeployment(t, nil)); err != nil {
		t.Errorf("a deployment with every required part was refused: %v", err)
	}
}

// The coordinator has no default for where its data lives: a deployment that
// leaves a backend unset is refused, naming it.
func TestValidate_RequiresEachBackend(t *testing.T) {
	if err := validate(testDeployment(t, func(d *deployment.Deployment, _ *Own) { d.Backends.Crons = nil })); err == nil || !strings.Contains(err.Error(), "Backends.Crons") {
		t.Fatalf("validate() = %v, want an error naming Backends.Crons", err)
	}
	if err := validate(testDeployment(t, func(_ *deployment.Deployment, o *Own) { o.Backends.Shards = nil })); err == nil || !strings.Contains(err.Error(), "Own.Backends.Shards") {
		t.Fatalf("validate() = %v, want an error naming Own.Backends.Shards", err)
	}
}

// testDeployment is a deployment with every required part set, over one
// in-memory database, after change has had its say.
func testDeployment(t *testing.T, change func(*deployment.Deployment, *Own)) (*deployment.Deployment, *Own) {
	t.Helper()
	st := mem.Open()
	d := &deployment.Deployment{
		Authn:  deployment.Authn{GRPC: noAuthn{}},
		Access: open.Access(),
		Backends: deployment.Backends{
			Tokens:         st.Tokens,
			Sources:        st.Sources,
			Crons:          st.Crons,
			Bins:           st.Bins,
			Notify:         st.Notify,
			IngestFailures: st.Ingest,
		},
		Mounts: &mounts.Backends{},
	}
	o := &Own{
		Orgs:     open.Orgs{},
		Platform: func(context.Context, string, string) (logdb.Platform, error) { return logdb.Platform{}, nil },
		Backends: Backends{
			Notices:      st.Notices,
			Coordination: st.Coord,
			Shards:       st.Shards,
			Services:     st.Sources,
		},
	}
	if change != nil {
		change(d, o)
	}
	return d, o
}

// noAuthn authenticates nobody: the tests never serve a call.
type noAuthn struct{ identity.Server }
