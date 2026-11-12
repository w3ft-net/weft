// Package hostedapp is hosted weft's adapter for the generic coordinator
// process (package app); the api process's is hostedapi. cmd/coordinator and cmd/api are
// thin mains that pass these to them; nothing in the generic packages imports
// this one. docs/plans/tenant-scope.md, Phase 4.
package hostedapp

import (
	"google.golang.org/grpc"

	"github.com/graybeardcorp/saaskit/email"

	"github.com/graybeardcorp/weft/app"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/identity"
	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/alerts"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
	"github.com/graybeardcorp/weft/tenancy/hosted/hostedapi"
	"github.com/graybeardcorp/weft/tenancy/hosted/nat"
	"github.com/graybeardcorp/weft/tenancy/hosted/retention"
)

// Coordinator is the adapter of the hosted coordinator: orgs are the tenants,
// teams and permissions decide what they may do, sessions come from the JWT,
// and archives expire by plan.
func Coordinator() app.Adapter {
	// The shell renders the tour's graduation notice like any other.
	onboarding.RegisterNotice()
	return app.Adapter{
		Coordinator:  Tenancy(),
		Identity:     Identity{},
		Coordination: hosted.Coordination,
		Notifier: func(db *store.Store) *coordinator.Notifier {
			return coordinator.NewNotifier(db, alerts.Targets(db, email.ConfigFromEnv())...)
		},
		Retention: func(db *store.Store, region string) logdb.Retention {
			return retention.New(db, region)
		},
		LegacyFactTable: hosted.LegacyFactTable,
		Start:           start,
	}
}

// Tenancy is who the hosted tenants are and what they may read and change.
// The hostedtest package installs the same as coordinator.DefaultAdapter for
// tests, and a test here keeps the two equal.
func Tenancy() coordinator.Adapter {
	return coordinator.Adapter{
		Orgs:       hosted.Orgs{},
		Scoper:     func(st *store.Store) tenancy.Scoper { return hosted.Scoper{Store: st} },
		Authorizer: func(st *store.Store) tenancy.Authorizer { return hosted.Authorizer{Store: st} },
	}
}

// start is the hosted work that needs the running coordinator: usage metering,
// the NAT route election and, with --customer, a dev tenant.
func start(rt app.Runtime) {
	// Of the coordinators running, one points the private subnet's default
	// route at itself.
	nat.Start(rt)

	// Start usage metering flush (every 30 seconds).
	rt.Coord.StartUsageFlush(rt.Ctx, rt.DB)

	// Seed dev data when running with a customer flag (dev/docker mode).
	if rt.Customer != "" {
		cust, _ := rt.DB.FindOrCreateOrg(rt.Customer)
		if cust != nil {
			rt.DB.SeedDevSubscription(cust.ID)
			rt.DB.SeedDevAgentToken(cust.ID, "dev-agent", rt.Customer)
		}
	}
}

// Identity authenticates callers by JWT and builds their session from the
// store.
type Identity struct{}

// Open reads the JWT secret, with the dev default and its warning.
func (Identity) Open(db *store.Store) identity.Server {
	return server{db: db, secret: hostedapi.JWTSecret()}
}

type server struct {
	db     *store.Store
	secret string
}

func (s server) Interceptors(skip identity.SkipMethods) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	build := func(uid, org string) (*auth.Session, error) { return auth.BuildSession(s.db, uid, org) }
	return []grpc.UnaryServerInterceptor{auth.UnaryServerInterceptor(build, s.secret, skip)},
		[]grpc.StreamServerInterceptor{auth.StreamServerInterceptor(build, s.secret, skip)}
}
