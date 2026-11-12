package hostedapi

import (
	"context"
	"errors"
	"net/http"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// The hosted product is the api's Identity: JWT sessions minted by a magic link
// or the CLI's device flow, a user who belongs to several orgs, a plan, and an
// onboarding tour through the demo org.
var _ weftapi.Identity = (*product)(nil)

// sessionOf is the hosted session the Identity resolved for the request: the
// full picture the core's Session is the neutral part of.
func sessionOf(r *http.Request) *auth.Session { return auth.SessionFromContext(r.Context()) }

// Authenticate validates the bearer JWT, builds the session in the org the
// X-Active-Org header selects (else the user's last, else their oldest) and
// remembers a changed choice.
func (p *product) Authenticate(next http.Handler) http.Handler {
	return auth.Middleware(p.Store(), p.secret, weftapi.RecordAccessIdentity)(next)
}

// Protect refuses an unverified email on routes with external effects. Used
// for token creation, integrations, source registration, team invites and
// namespace creation: anything an unverified attacker could abuse before the
// cleanup sweep drops their account. Read-only routes and the Stripe billing
// flow stay open for unverified users so they can still set up payment without
// a second round-trip to their inbox.
func (p *product) Protect(next http.Handler) http.Handler {
	return auth.RequireEmailVerified(next)
}

// Entitled is true while the org has an active subscription.
func (p *product) Entitled(_ context.Context, s weftapi.Session) error {
	if sub, _ := p.Store().ActiveSubscription(s.Org()); sub == nil {
		return errors.New("no active subscription")
	}
	return nil
}

// Forward carries the caller's JWT, and the org they selected, to the
// coordinator, whose own interceptor validates the same bearer token.
func (p *product) Forward(r *http.Request) map[string]string {
	out := map[string]string{}
	if a := r.Header.Get("Authorization"); a != "" {
		out["authorization"] = a
	}
	if org := r.Header.Get(auth.ActiveOrgHeader); org != "" {
		out["x-active-org"] = org
	}
	return out
}

// ShellRan advances the onboarding lesson the finished pipeline completes and
// tells the client the step it moved to, as the shell stream's tour_advance event.
func (p *product) ShellRan(ctx context.Context, _ weftapi.Session, run weftapi.ShellRun) map[string]any {
	trigger := onboarding.TriggerExec
	if run.Modal {
		trigger = onboarding.TriggerModalFetch
	}
	step, prompt, advanced := p.maybeAdvanceShellLesson(ctx, auth.SessionFromContext(ctx), run.Pipeline, nil, trigger)
	if !advanced {
		return nil
	}
	return map[string]any{"type": "tour_advance", "step": step, "prompt": prompt}
}

// Routes registers sign-in (magic link and device flow), the account, invite
// redemption and the onboarding tour, with the limits they had in the core.
func (p *product) identityRoutes(rt *weftapi.Routes) {
	cfg := rt.Config()
	guarded := func(route weftapi.RouteLimit, bodyMax int64, h http.HandlerFunc) http.Handler {
		return weftapi.BodyCap(bodyMax, rt.Limit(route, h))
	}

	rt.Public("POST /api/auth/login", guarded(weftapi.LimitLogin, cfg.AuthBodyMax, p.handleLogin))
	rt.Public("POST /api/auth/signup", guarded(weftapi.LimitAnonAuth, cfg.AuthBodyMax, p.handleSignup))
	rt.Public("POST /api/auth/verify", guarded(weftapi.LimitAnonAuth, cfg.AuthBodyMax, p.handleVerify))
	rt.Public("POST /api/auth/device", guarded(weftapi.LimitAnonAuth, cfg.AuthBodyMax, p.handleDeviceInit))
	rt.Public("POST /api/auth/device/poll", guarded(weftapi.LimitDevicePoll, cfg.AuthBodyMax, p.handleDevicePoll))
	rt.Public("POST /api/team/accept", guarded(weftapi.LimitAnonAuth, cfg.AuthBodyMax, p.handleAcceptInvite))

	rt.Authed("GET /api/auth/me", http.HandlerFunc(p.handleMe))
	rt.Authed("POST /api/auth/device/approve", http.HandlerFunc(p.handleDeviceApprove))
	rt.Authed("POST /api/auth/resend-verification", http.HandlerFunc(p.handleResendVerification))
	rt.Authed("POST /api/account/welcome-dismiss", http.HandlerFunc(p.handleWelcomeDismiss))
	rt.Authed("POST /api/onboarding/advance", http.HandlerFunc(p.handleOnboardingAdvance))
	rt.Authed("POST /api/onboarding/dismiss", http.HandlerFunc(p.handleOnboardingDismiss))
	rt.Authed("POST /api/onboarding/graduation-seen", http.HandlerFunc(p.handleGraduationNoticeSeen))
	rt.Authed("POST /api/shell/builtin", http.HandlerFunc(p.handleShellBuiltin))
}

// Routes is Identity.Routes: the sign-in and account routes. The rest of the
// product's routes (billing, team, ...) come through product.routes.
func (p *product) Routes(rt *weftapi.Routes) { p.identityRoutes(rt) }
