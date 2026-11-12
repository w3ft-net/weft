// Package hostedapi is hosted weft's adapter for the generic api process
// (package app/apiapp): the product's billing, Slack and account-cleanup
// configuration. It lives apart from hostedapp because cmd/api is built
// without cgo, and hostedapp reaches the coordinator's DuckDB closure.
package hostedapi

import (
	"context"
	"os"
	"strings"

	weftsdk "github.com/w3ft-net/weft-go"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/app/apiapp"
	"github.com/graybeardcorp/weft/store"
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

// API is the adapter of the hosted api: the product's billing, Slack and
// account-cleanup configuration.
func API() apiapp.Adapter {
	return apiapp.Adapter{
		JWTSecret: JWTSecret,
		Configure: configureProduct,
		Start: func(ctx context.Context, srv *weftapi.Server) {
			// Start the periodic cleanup of accounts that were created but never
			// completed email verification. See auth.UnverifiedGracePeriod.
			srv.StartUnverifiedSweep(ctx)
		},
		Serving: func(ctx context.Context, srv *weftapi.Server) {
			// Start the daily Stripe usage reporter. Fires at 03:00 UTC and
			// posts yesterday's ingestion to the metered overage prices for
			// each customer with one. No-op when STRIPE_SECRET_KEY is unset
			// or when a tier has no overage price configured.
			srv.StartUsageReporter(ctx)
		},
	}
}

// configureProduct reads Stripe prices and keys and the Slack client from the
// environment. Each tier has a flat recurring price plus an optional metered
// overage price. Absence of the _OVERAGE variant puts that tier in flat-only
// mode — see docs/plans/stripe.md for the overage-billing plan.
func configureProduct(srv *weftapi.Server) {
	for _, tier := range []store.Tier{store.TierSwatch, store.TierYard, store.TierBolt} {
		prefix := "STRIPE_PRICE_" + strings.ToUpper(string(tier))
		flat := os.Getenv(prefix)
		if flat == "" {
			continue
		}
		srv.StripePrices[tier] = weftapi.TierPrices{
			Flat:    flat,
			Overage: os.Getenv(prefix + "_OVERAGE"),
		}
	}
	srv.StripeSecretKey = os.Getenv("STRIPE_SECRET_KEY")
	srv.StripePublishableKey = os.Getenv("STRIPE_PUBLISHABLE_KEY")
	srv.StripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")
	srv.SlackClientID = os.Getenv("SLACK_CLIENT_ID")
	srv.SlackClientSecret = os.Getenv("SLACK_CLIENT_SECRET")
}
