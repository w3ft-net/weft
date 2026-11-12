package hostedapi

import (
	"context"
	"net/http"
	"os"
	"strings"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// TierPrices is a tier's flat-rate and metered-overage Stripe price
// pair. Overage may be empty — such a tier's subscriptions get a
// single flat item and no meter events are emitted for it.
type TierPrices struct {
	Flat    string
	Overage string
}

// product is the hosted product's surface on the api server: billing (Stripe),
// the platform operator's admin routes, the Slack, PagerDuty and webhook
// integrations, and the team: its members, invites, permissions and the shell's
// adduser/deluser/usermod/groups/id, plus notices and shared links. It is what
// the core api left out of its own routes.
type product struct {
	*weftapi.Server

	// secret signs and checks the JWTs the product mints for its users.
	secret string
	// email is where mail is sent from; nil (dev mode) logs the link instead.
	email *EmailConfig
	// tierPurchasable says whether signup may sell a tier.
	tierPurchasable func(store.Tier) bool

	StripeSecretKey      string
	StripePublishableKey string
	StripeWebhookSecret  string
	// StripePrices maps each paid tier to its flat + overage price IDs.
	// A tier absent from the map, or with an empty Overage, is in
	// flat-only mode: subscriptions get a single item and no usage is
	// reported. Useful for piloting Stripe overage config one tier at
	// a time. See docs/plans/stripe.md.
	StripePrices map[store.Tier]TierPrices

	// postMeterEvent is the Stripe meter-events client — see
	// usage_reporter.go. Overridable in tests; production wiring
	// happens in StartUsageReporter.
	postMeterEvent postMeterEventFunc

	// Slack OAuth config
	SlackClientID     string
	SlackClientSecret string
}

func newProduct() *product {
	return &product{StripePrices: make(map[store.Tier]TierPrices)}
}

// attach binds the product to the server it serves.
func (p *product) attach(srv *weftapi.Server) {
	p.Server = srv
	onboarding.RegisterNotice()
	srv.AddShellCommand(func(ctx context.Context, _ weftapi.Session, pipeline string) (bool, string, error) {
		return p.maybeHandleTeamCommand(ctx, auth.SessionFromContext(ctx), pipeline)
	})
	p.tierPurchasable = func(t store.Tier) bool {
		_, ok := p.StripePrices[t]
		return ok
	}
}

// configure reads Stripe prices and keys and the Slack client from the
// environment. Each tier has a flat recurring price plus an optional metered
// overage price. Absence of the _OVERAGE variant puts that tier in flat-only
// mode — see docs/plans/stripe.md for the overage-billing plan.
func (p *product) configure(srv *weftapi.Server) {
	p.attach(srv)
	for _, tier := range []store.Tier{store.TierSwatch, store.TierYard, store.TierBolt} {
		prefix := "STRIPE_PRICE_" + strings.ToUpper(string(tier))
		flat := os.Getenv(prefix)
		if flat == "" {
			continue
		}
		p.StripePrices[tier] = TierPrices{
			Flat:    flat,
			Overage: os.Getenv(prefix + "_OVERAGE"),
		}
	}
	p.StripeSecretKey = os.Getenv("STRIPE_SECRET_KEY")
	p.StripePublishableKey = os.Getenv("STRIPE_PUBLISHABLE_KEY")
	p.StripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")
	p.SlackClientID = os.Getenv("SLACK_CLIENT_ID")
	p.SlackClientSecret = os.Getenv("SLACK_CLIENT_SECRET")
}

// routes registers the product's routes: the same paths, authentication and
// limits they had in the core api.
func (p *product) routes(rt *weftapi.Routes) {
	protected := func(h http.HandlerFunc) http.Handler { return p.Protect(h) }

	// Stripe handles its own body parsing + webhook-secret verification;
	// only the rate limit is added here (Stripe retries with backoff on
	// 429, so this is safe).
	rt.Public("POST /api/billing/webhook", rt.Limit(weftapi.LimitStripeWebhook, http.HandlerFunc(p.handleStripeWebhook)))
	rt.Public("GET /api/integrations/slack/callback", http.HandlerFunc(p.handleSlackCallback))

	rt.Authed("POST /api/billing/checkout", http.HandlerFunc(p.handleCheckout))
	rt.Authed("POST /api/billing/setup-intent", http.HandlerFunc(p.handleSetupIntent))
	rt.Authed("GET /api/billing/usage", http.HandlerFunc(p.handleUsage))

	rt.Authed("GET /api/integrations", http.HandlerFunc(p.handleListIntegrations))
	rt.Authed("POST /api/integrations/slack/connect", protected(p.handleSlackConnect))
	rt.Authed("DELETE /api/integrations/slack", protected(p.handleSlackDisconnect))
	rt.Authed("PUT /api/integrations/pagerduty", protected(p.handlePagerDutySave))
	rt.Authed("DELETE /api/integrations/pagerduty", protected(p.handlePagerDutyDisconnect))
	rt.Authed("DELETE /api/integrations/pagerduty/{name}", protected(p.handlePagerDutyDelete))

	rt.Authed("GET /api/team", http.HandlerFunc(p.handleListTeam))
	rt.Authed("PUT /api/team/name", http.HandlerFunc(p.handleUpdateTeamName))
	rt.Authed("POST /api/team/invite", protected(p.handleInviteTeamMember))
	rt.Authed("DELETE /api/team/{id}", http.HandlerFunc(p.handleRemoveTeamMember))
	rt.Authed("PUT /api/namespaces/{name}/permissions/{userId}", protected(p.handleSetNamespacePermission))
	rt.Authed("GET /api/permissions", http.HandlerFunc(p.handleGetPermissions))
	rt.Authed("PUT /api/permissions", protected(p.handleSetPermissions))

	rt.Authed("GET /api/notices", http.HandlerFunc(p.handleListNotices))
	rt.Authed("POST /api/notices/{id}/seen", http.HandlerFunc(p.handleNoticeSeen))
	rt.Authed("GET /api/shared-links/{slug}", http.HandlerFunc(p.handleGetSharedLink))

	// Admin routes (superadmin only).
	admin := http.NewServeMux()
	admin.HandleFunc("POST /api/admin/customers", p.handleAdminCreateCustomer)
	admin.HandleFunc("GET /api/admin/customers", p.handleAdminListCustomers)
	admin.HandleFunc("GET /api/admin/customers/{name}", p.handleAdminGetCustomer)
	admin.HandleFunc("POST /api/admin/customers/{name}/comp", p.handleAdminSetComp)
	admin.HandleFunc("POST /api/admin/customers/{name}/tier", p.handleAdminSetTier)
	admin.HandleFunc("GET /api/admin/buckets", p.handleAdminListBuckets)
	admin.HandleFunc("GET /api/admin/buckets/{id}", p.handleAdminGetBucket)
	admin.HandleFunc("PUT /api/admin/buckets/{id}", p.handleAdminUpdateBucket)
	admin.HandleFunc("GET /api/admin/buckets/{id}/shards", p.handleAdminListShards)
	admin.HandleFunc("POST /api/admin/shards/{id}/archive", p.handleAdminArchiveShard)
	admin.HandleFunc("DELETE /api/admin/shards/{id}", p.handleAdminDeleteShard)
	rt.Public("/api/admin/", rt.Auth(p.requireSuperadmin(admin)))
}

// requireSuperadmin gates admin endpoints on the Authorizer's superadmin
// action, for the Session that AuthMiddleware already materialized.
func (p *product) requireSuperadmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := sessionOf(r)
		if session == nil || p.Authorize(r.Context(), session.Caller(), hosted.ActionSuperadmin, tenancy.Resource{}) != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
