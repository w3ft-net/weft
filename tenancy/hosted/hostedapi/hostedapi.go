// Package hostedapi is hosted weft's adapter for the generic api process
// (package app/apiapp): the product's identity (sign-in, accounts, onboarding),
// billing, teams, integrations and account cleanup. It lives apart from hostedapp because cmd/api is built
// without cgo, and hostedapp reaches the coordinator's DuckDB closure.
package hostedapi

import (
	"context"
	"os"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/alerting"
	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/app/apiapp"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/alerts"
)

// JWTSecret is JWT_SECRET, or a development default that is reported as one.
func JWTSecret() string {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "dev-secret-change-in-production"
		weftsdk.Warnf(context.Background(), "using default JWT secret — set JWT_SECRET in production")
	}
	return secret
}

// API is the adapter of the hosted api: its identity (JWT sessions, magic links,
// the device flow, onboarding), billing, teams, Slack and account cleanup.
func API() apiapp.Adapter {
	p := newProduct()
	return apiapp.Adapter{
		Identity: func(*store.Store) (weftapi.Identity, error) {
			p.secret = JWTSecret()
			p.email = EmailConfigFromEnv()
			return p, nil
		},
		Tenancy: hosted.NewAdapter,
		// The api only validates a save of notify.yaml against these kinds
		// (Open reads no credential), so it needs no SMTP settings.
		AlertTargets: func(db *store.Store) []alerting.AlertTarget { return alerts.Targets(db, nil) },
		Configure:    p.configure,
		Routes:       p.routes,
		Start: func(ctx context.Context, srv *weftapi.Server) {
			// Start the periodic cleanup of accounts that were created but never
			// completed email verification. See auth.UnverifiedGracePeriod.
			p.StartUnverifiedSweep(ctx)
		},
		Serving: func(ctx context.Context, srv *weftapi.Server) {
			// Start the daily Stripe usage reporter. Fires at 03:00 UTC and
			// posts yesterday's ingestion to the metered overage prices for
			// each customer with one. No-op when STRIPE_SECRET_KEY is unset
			// or when a tier has no overage price configured.
			p.StartUsageReporter(ctx)
		},
	}
}
