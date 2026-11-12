package hostedapp_test

import (
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/identity"
	"github.com/graybeardcorp/weft/notices"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/hostedapp"
)

// The hosted coordinator must be built with the hosted Authorizer. A coordinator
// built without one acts under coordinator.DefaultAdapter's, which is the open
// adapter's and allows every action the core defines: the shell mounts would
// then skip the team and ownership gates. This goes through the same
// app.Adapter.WithTenancy that app.Coordinator builds its config with.
func TestCoordinator_IsBuiltWithTheHostedGates(t *testing.T) {
	var db *store.Store
	cfg := hostedapp.Coordinator().WithTenancy(db, coordinator.CoordinatorConfig{Compute: &compute.Engine{}})

	if _, ok := cfg.Orgs.(hosted.Orgs); !ok {
		t.Errorf("Orgs = %T, want hosted.Orgs", cfg.Orgs)
	}
	if _, ok := cfg.Scoper.(hosted.Scoper); !ok {
		t.Errorf("Scoper = %T, want hosted.Scoper", cfg.Scoper)
	}
	if _, ok := cfg.Authz.(hosted.Authorizer); !ok {
		t.Fatalf("Authz = %T, want hosted.Authorizer", cfg.Authz)
	}

	c := coordinator.NewCoordinator(cfg)
	if _, ok := c.Authz.(hosted.Authorizer); !ok {
		t.Errorf("the coordinator's Authz = %T, want hosted.Authorizer: it fell back to the open default", c.Authz)
	}
	if _, ok := c.Scoper.(hosted.Scoper); !ok {
		t.Errorf("the coordinator's Scoper = %T, want hosted.Scoper", c.Scoper)
	}
}

func TestIdentity_YieldsAnInterceptorOfEachKind(t *testing.T) {
	t.Setenv("JWT_SECRET", "s")
	var db *store.Store
	unary, stream := hostedapp.Identity{}.Open(db).Interceptors(identity.SkipMethods{"/x/Y": true})
	if len(unary) != 1 || len(stream) != 1 {
		t.Fatalf("got %d unary and %d stream interceptors, want one of each", len(unary), len(stream))
	}
}

// The shell renders the tour's graduation notice, and the registry has no such
// entry of its own: a hosted coordinator that forgot to register it would leave
// the row pending forever.
func TestCoordinator_RendersTheGraduationNotice(t *testing.T) {
	hostedapp.Coordinator()
	n, ok := notices.Render(store.PendingNotice{Key: onboarding.GraduatedKey})
	if !ok || n.Title == "" {
		t.Fatalf("graduation notice did not render: %+v, %v", n, ok)
	}
}
