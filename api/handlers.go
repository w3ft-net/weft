package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/internal/timerange"

	"github.com/google/uuid"
	"github.com/graybeardcorp/saaskit/stripekit"
	"github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/customer"
	"github.com/stripe/stripe-go/v85/subscription"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
	"gorm.io/gorm"
)

// unverifiedSweepInterval is how often the cleanup goroutine deletes
// expired unverified accounts. Must be comfortably shorter than
// UnverifiedGracePeriod so an expired account doesn't linger.
const unverifiedSweepInterval = 1 * time.Hour

// Server is the weft API server.
type Server struct {
	store     *store.Store
	authz     tenancy.Authorizer // decides every action gate; the hosted one over store by default
	jwtSecret string
	coordAddr string       // coordinator gRPC address
	baseURL   string       // e.g., https://w3ft.net
	email     *EmailConfig // nil = dev mode (log links)

	// Coordinator gRPC client (connected lazily via Connect).
	coordConn   *grpc.ClientConn
	coordClient pb.WeftCoordinatorClient

	// Stripe config
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
	// api/usage_reporter.go. Overridable in tests; production wiring
	// happens in StartUsageReporter.
	postMeterEvent postMeterEventFunc

	// Slack OAuth config
	SlackClientID     string
	SlackClientSecret string

	// Runtime config pointer — populated in Handler() via SetConfig.
	// Serves both rate-limit middleware and body-cap sizing. Nil means
	// dev/test mode: defaults apply, no live refresh.
	cfgPtr  *atomic.Pointer[config.Runtime]
	limiter *limiter

	// classifier drives LLM-based schema-field classification for
	// pending sources (handleAgentSample, accumulatePendingSamples
	// — see ingest/llmdetect.go). Nil means disabled: ClassifyAsync
	// falls back to the deterministic envelope/JSON detection only, same
	// as before this existed. Populated via SetClassifier.
	classifier ingest.Classifier
}

// SetClassifier wires the LLM classifier used for pending-source schema
// detection. Optional — a nil/never-called classifier just means
// ClassifyAsync skips LLM classification (see NewBedrockClassifierFromEnv,
// which itself returns nil when unconfigured).
func (s *Server) SetClassifier(c ingest.Classifier) {
	s.classifier = c
}

// SetConfig wires a live runtime-config pointer to the server. The
// API main binary calls this right after NewServer with the watcher
// from config.Watch. Tests can omit it and get compile-time defaults.
func (s *Server) SetConfig(ptr *atomic.Pointer[config.Runtime]) {
	s.cfgPtr = ptr
	s.limiter = newLimiter(ptr)
}

// currentConfig returns the live runtime config snapshot.
func (s *Server) currentConfig() *config.Runtime {
	return s.cfgPtr.Load()
}

// apiBaseURL returns the API's own external URL (e.g., https://api.w3ft.net).
// Derived from baseURL by inserting "api." prefix.
func (s *Server) apiBaseURL() string {
	// https://w3ft.net → https://api.w3ft.net
	return strings.Replace(s.baseURL, "://", "://api.", 1)
}

// TierPrices is a tier's flat-rate and metered-overage Stripe price
// pair. Overage may be empty — such a tier's subscriptions get a
// single flat item and no meter events are emitted for it.
type TierPrices struct {
	Flat    string
	Overage string
}

// NewServer creates an API server. Wires a defaults-backed config
// pointer + limiter so callers that don't call SetConfig (tests,
// dev) still have a live limiter — production overwrites both via
// SetConfig before serving.
func NewServer(s *store.Store, jwtSecret, coordAddr, baseURL string) *Server {
	cfgPtr := new(atomic.Pointer[config.Runtime])
	cfgPtr.Store(config.Defaults())
	return &Server{
		store:        s,
		authz:        hosted.Authorizer{Store: s},
		jwtSecret:    jwtSecret,
		coordAddr:    coordAddr,
		baseURL:      baseURL,
		email:        EmailConfigFromEnv(),
		StripePrices: make(map[store.Tier]TierPrices),
		cfgPtr:       cfgPtr,
		limiter:      newLimiter(cfgPtr),
	}
}

// authorize asks the Authorizer whether the session's user may take act. Any
// error is a refusal.
func (s *Server) authorize(ctx context.Context, session *auth.Session, act tenancy.Action, res tenancy.Resource) error {
	return s.authz.Authorize(ctx, hosted.Actor{Org: session.ActiveOrgID, User: session.UserID}, act, res)
}

// Connect establishes the gRPC connection to the coordinator. It should be
// called once before serving requests. The caller is responsible for calling
// Close when done.
func (s *Server) Connect() error {
	conn, err := grpc.NewClient(s.coordAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(weftsdk.GRPC.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(weftsdk.GRPC.StreamClientInterceptor()))
	if err != nil {
		return fmt.Errorf("connect to coordinator at %s: %w", s.coordAddr, err)
	}
	s.coordConn = conn
	s.coordClient = pb.NewWeftCoordinatorClient(conn)
	return nil
}

// Close releases the coordinator gRPC connection.
func (s *Server) Close() {
	if s.coordConn != nil {
		s.coordConn.Close()
	}
}

// coordCtx returns a context suitable for calling the coordinator on
// behalf of the authenticated caller of r. The caller's JWT is forwarded
// as outgoing gRPC metadata so the coordinator's auth interceptor sees
// the same bearer token the api middleware already validated.
func (s *Server) coordCtx(r *http.Request) context.Context {
	ctx := r.Context()
	if auth := r.Header.Get("Authorization"); auth != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", auth)
	}
	if org := r.Header.Get("X-Active-Org"); org != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-active-org", org)
	}
	return ctx
}

// StartUnverifiedSweep starts a background goroutine that periodically
// deletes accounts that were created but never verified within the grace
// period. Blocks until ctx is cancelled. Cascading FK deletes take care
// of the user's customer/subscription/team-member/tokens/sources.
func (s *Server) StartUnverifiedSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(unverifiedSweepInterval)
		defer ticker.Stop()
		// Run once at startup so restarts don't give squatters a free
		// extra interval. Ignore errors — we'll retry next tick.
		s.sweepUnverified(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepUnverified(ctx)
			}
		}
	}()
}

func (s *Server) sweepUnverified(ctx context.Context) {
	cutoff := time.Now().Add(-auth.UnverifiedGracePeriod)
	result := s.store.DB().
		Where("email_verified_at IS NULL AND created_at < ?", cutoff).
		Delete(&store.User{})
	n, err := result.RowsAffected, result.Error
	if err != nil {
		weftsdk.Infof(ctx, "unverified sweep: %v", err)
		return
	}
	if n > 0 {
		weftsdk.Infof(ctx, "unverified sweep: deleted %d expired accounts", n)
	}
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	cfg := s.currentConfig()

	// guarded stacks body-cap + rate-limit on a public endpoint. Body
	// caps come first so oversize requests don't even touch the
	// limiter's bucket state.
	guarded := func(route RouteLimit, bodyMax int64, h http.HandlerFunc) http.Handler {
		return BodyCap(bodyMax, s.limiter.RateLimit(route, h))
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Public routes (no auth) — each guarded per the audit §1 budgets.
	mux.Handle("POST /api/auth/login", guarded(LimitLogin, cfg.AuthBodyMax, s.handleLogin))
	mux.Handle("POST /api/auth/signup", guarded(LimitAnonAuth, cfg.AuthBodyMax, s.handleSignup))
	mux.Handle("POST /api/auth/verify", guarded(LimitAnonAuth, cfg.AuthBodyMax, s.handleVerify))
	mux.Handle("POST /api/auth/device", guarded(LimitAnonAuth, cfg.AuthBodyMax, s.handleDeviceInit))
	mux.Handle("POST /api/auth/device/poll", guarded(LimitDevicePoll, cfg.AuthBodyMax, s.handleDevicePoll))
	// Stripe handles its own body parsing + webhook-secret verification;
	// only the rate limit is added here (Stripe retries with backoff on
	// 429, so this is safe).
	mux.Handle("POST /api/billing/webhook", s.limiter.RateLimit(LimitStripeWebhook, http.HandlerFunc(s.handleStripeWebhook)))

	// Authenticated routes.
	// protected wraps a handler in RequireEmailVerified. Used for routes
	// that touch external effects (token creation, integrations, source
	// registration, team invites, namespace creation) — anything an
	// unverified attacker could abuse to do real damage before the cleanup
	// sweep drops their account. Read-only routes and the Stripe billing
	// flow stay open for unverified users so they can still set up
	// payment without a second round-trip to their inbox.
	protected := func(h http.HandlerFunc) http.Handler {
		return RequireEmailVerified(h)
	}

	auth := http.NewServeMux()
	auth.HandleFunc("GET /api/auth/me", s.handleMe)
	auth.HandleFunc("POST /api/auth/device/approve", s.handleDeviceApprove)
	auth.HandleFunc("POST /api/auth/resend-verification", s.handleResendVerification)
	auth.HandleFunc("POST /api/account/welcome-dismiss", s.handleWelcomeDismiss)
	auth.HandleFunc("POST /api/onboarding/advance", s.handleOnboardingAdvance)
	auth.HandleFunc("POST /api/onboarding/dismiss", s.handleOnboardingDismiss)
	auth.HandleFunc("POST /api/onboarding/graduation-seen", s.handleGraduationNoticeSeen)
	auth.HandleFunc("GET /api/notices", s.handleListNotices)
	auth.HandleFunc("POST /api/notices/{id}/seen", s.handleNoticeSeen)
	auth.HandleFunc("GET /api/fs/list", s.handleListFS)
	auth.HandleFunc("GET /api/fs/read", s.handleReadFS)
	auth.HandleFunc("PUT /api/fs/write", s.handleWriteFS)
	auth.HandleFunc("DELETE /api/fs/remove", s.handleRemoveFS)
	auth.HandleFunc("POST /api/billing/checkout", s.handleCheckout)
	auth.HandleFunc("POST /api/billing/setup-intent", s.handleSetupIntent)
	auth.HandleFunc("GET /api/billing/usage", s.handleUsage)
	auth.HandleFunc("GET /api/tokens", s.handleListTokens)
	auth.Handle("POST /api/tokens", protected(s.handleCreateToken))
	auth.Handle("DELETE /api/tokens/{id}", protected(s.handleRevokeToken))
	auth.HandleFunc("GET /api/apps", s.handleListApps)
	auth.HandleFunc("GET /api/ingestion-status", s.handleIngestionStatus)
	auth.Handle("POST /api/shell/exec", BodyCapFunc(cfg.ShellExecBodyMax, s.handleShellExec))
	auth.HandleFunc("POST /api/shell/builtin", s.handleShellBuiltin)
	auth.HandleFunc("GET /api/functions", s.handleListFunctions)
	auth.HandleFunc("GET /api/functions/{name}", s.handleGetFunction)
	auth.Handle("PUT /api/functions/{name}", BodyCap(cfg.FunctionBodyMax, protected(s.handleSaveFunction)))
	auth.Handle("DELETE /api/functions/{name}", protected(s.handleDeleteFunction))
	auth.HandleFunc("GET /api/crons", s.handleListWatches)
	auth.HandleFunc("GET /api/crons/{name}", s.handleGetWatch)
	auth.Handle("PUT /api/crons/{name}", BodyCap(cfg.WatchBodyMax, protected(s.handleSaveWatch)))
	auth.Handle("DELETE /api/crons/{name}", protected(s.handleDeleteWatch))
	auth.HandleFunc("GET /api/crons/{name}/results", s.handleWatchResults)
	auth.HandleFunc("GET /api/crons/{name}/latest", s.handleWatchLatest)
	auth.HandleFunc("GET /api/crons/{name}/chart", s.handleWatchChart)
	auth.HandleFunc("GET /api/shared-links/{slug}", s.handleGetSharedLink)
	auth.HandleFunc("GET /api/dashboards", s.handleListDashboards)
	auth.HandleFunc("GET /api/dashboards/{name}", s.handleGetDashboard)
	auth.Handle("PUT /api/dashboards/{name}", BodyCap(cfg.WatchBodyMax, protected(s.handleSaveDashboard)))
	auth.Handle("DELETE /api/dashboards/{name}", protected(s.handleDeleteDashboard))
	auth.HandleFunc("GET /api/notify", s.handleGetNotifyConfig)
	auth.Handle("PUT /api/notify", BodyCap(cfg.WatchBodyMax, protected(s.handleSaveNotifyConfig)))
	auth.HandleFunc("GET /api/integrations", s.handleListIntegrations)
	auth.Handle("POST /api/integrations/slack/connect", protected(s.handleSlackConnect))
	auth.Handle("DELETE /api/integrations/slack", protected(s.handleSlackDisconnect))
	auth.Handle("PUT /api/integrations/pagerduty", protected(s.handlePagerDutySave))
	auth.Handle("DELETE /api/integrations/pagerduty", protected(s.handlePagerDutyDisconnect))
	auth.Handle("DELETE /api/integrations/pagerduty/{name}", protected(s.handlePagerDutyDelete))
	auth.Handle("PUT /api/integrations/webhook-secrets", protected(s.handleWebhookSecretSave))
	auth.Handle("DELETE /api/integrations/webhook-secrets/{name}", protected(s.handleWebhookSecretDelete))
	auth.HandleFunc("GET /api/namespaces", s.handleListNamespaces)
	auth.Handle("POST /api/namespaces", protected(s.handleCreateNamespace))
	auth.Handle("DELETE /api/namespaces/{name}", protected(s.handleDeleteNamespace))
	auth.Handle("PUT /api/namespaces/{name}/permissions/{userId}", protected(s.handleSetNamespacePermission))
	auth.HandleFunc("GET /api/permissions", s.handleGetPermissions)
	auth.Handle("PUT /api/permissions", protected(s.handleSetPermissions))
	auth.HandleFunc("GET /api/team", s.handleListTeam)
	auth.HandleFunc("PUT /api/team/name", s.handleUpdateTeamName)
	auth.Handle("POST /api/team/invite", protected(s.handleInviteTeamMember))
	auth.HandleFunc("DELETE /api/team/{id}", s.handleRemoveTeamMember)

	// Source onboarding (dashboard-authenticated).
	auth.HandleFunc("GET /api/sources/pending", s.handlePendingSources)
	auth.HandleFunc("GET /api/sources/pending/count", s.handlePendingSourcesCount)
	auth.Handle("POST /api/sources/{namespace}/{app}/configure", protected(s.handleConfigureSource))
	auth.Handle("POST /api/sources/{namespace}/{app}/schema-preview", protected(s.handleSchemaPreview))
	auth.Handle("POST /api/sources/{namespace}/{app}/resample", protected(s.handleResampleSource))
	auth.Handle("POST /api/sources/{namespace}/{app}/probe", protected(s.handleProbeSource))
	auth.HandleFunc("GET /api/sources/{namespace}/{app}/schema", s.handleSourceSchema)

	// Source management (dashboard-authenticated).
	auth.HandleFunc("GET /api/sources", s.handleListSources)
	auth.Handle("POST /api/sources/kinesis", protected(s.handleAddKinesisSource))
	auth.Handle("DELETE /api/sources/{namespace}/{app}", protected(s.handleDeleteSource))

	// Public routes (no auth).
	mux.Handle("POST /api/team/accept", guarded(LimitAnonAuth, cfg.AuthBodyMax, s.handleAcceptInvite))
	mux.HandleFunc("GET /api/integrations/slack/callback", s.handleSlackCallback)

	// Heroku log drain: customer adds a drain via `heroku drains:add
	// https://api.w3ft.net/api/ingest/heroku/<token>`. Auth is the
	// {token} URL path component (Heroku doesn't speak Bearer
	// headers); the handler validates against agent_tokens, parses
	// the RFC5424 octet-counted body, and forwards to the coordinator
	// via the existing IngestStream gRPC. See docs/plans/http-ingest.md.
	mux.Handle("POST /api/ingest/heroku/{token}", s.IngestHandler(HerokuAdapter{}))

	// CloudWatch push ingestion: a customer-owned Kinesis Data Firehose
	// delivery stream (CloudFormation-deployed, see the plan) posts
	// here after a CloudWatch Logs subscription filter feeds it. Auth
	// is the X-Amz-Firehose-Access-Key header (the existing wft_...
	// namespace token); response shape is Firehose-specific (200 +
	// {requestId, timestamp}), handled by CloudWatchAdapter.WriteSuccess.
	// See docs/plans/cloudwatch-kinesis-subscription.md.
	mux.Handle("POST /api/ingest/cloudwatch", s.IngestHandler(CloudWatchAdapter{}))

	// OTLP/HTTP metrics (JSON), kept to system metrics. Render's Custom
	// metrics destination is configured with the base URL
	// https://api.w3ft.net/api/ingest/otlp and a weft token as its API key;
	// Render appends /v1/metrics itself. See docs/plans/render-metrics.md.
	mux.Handle("POST /api/ingest/otlp/v1/metrics", s.IngestHandler(OTLPAdapter{}))

	// Admin routes (superadmin only).
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("POST /api/admin/customers", s.handleAdminCreateCustomer)
	adminMux.HandleFunc("GET /api/admin/customers", s.handleAdminListCustomers)
	adminMux.HandleFunc("GET /api/admin/customers/{name}", s.handleAdminGetCustomer)
	adminMux.HandleFunc("POST /api/admin/customers/{name}/comp", s.handleAdminSetComp)
	adminMux.HandleFunc("POST /api/admin/customers/{name}/tier", s.handleAdminSetTier)
	adminMux.HandleFunc("GET /api/admin/buckets", s.handleAdminListBuckets)
	adminMux.HandleFunc("GET /api/admin/buckets/{id}", s.handleAdminGetBucket)
	adminMux.HandleFunc("PUT /api/admin/buckets/{id}", s.handleAdminUpdateBucket)
	adminMux.HandleFunc("GET /api/admin/buckets/{id}/shards", s.handleAdminListShards)
	adminMux.HandleFunc("POST /api/admin/shards/{id}/archive", s.handleAdminArchiveShard)
	adminMux.HandleFunc("DELETE /api/admin/shards/{id}", s.handleAdminDeleteShard)
	mux.Handle("/api/admin/", AuthMiddleware(s.store, s.jwtSecret, s.requireSuperadmin(adminMux)))

	// Agent-facing routes (agent token auth).
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("POST /api/agent/sample", s.handleAgentSample)
	agentMux.HandleFunc("GET /api/agent/source-status", s.handleAgentSourceStatus)
	mux.Handle("/api/agent/", AgentTokenMiddleware(s.store, agentMux))

	// Wrap the user-facing inner mux with the client-version gate. Every
	// AuthMiddleware(... , auth) below picks this up automatically, so
	// the gate applies to every JWT-bound route without having to enumerate
	// them. Pass-through when MIN_WEFT_CLI_VERSION is unset / header is
	// missing — see ClientVersionMiddleware for the policy.
	gatedAuth := ClientVersionMiddleware(auth)

	// Dashboard-authenticated route registrations.
	mux.Handle("/api/sources", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/sources/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))

	mux.Handle("/api/auth/me", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/auth/device/approve", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/auth/resend-verification", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/billing/checkout", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/billing/setup-intent", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/billing/usage", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/tokens", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/tokens/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/apps", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/shell/exec", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/shell/builtin", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/functions", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/functions/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/crons", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/crons/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/shared-links/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/dashboards", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/dashboards/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/integrations", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/integrations/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/notify", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/namespaces", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/namespaces/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/permissions", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/team", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/team/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/onboarding/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/notices", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/notices/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/fs/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/account/", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))
	mux.Handle("/api/ingestion-status", AuthMiddleware(s.store, s.jwtSecret, gatedAuth))

	return weftsdk.TraceMiddleware(AccessLog(CORSMiddleware(s.baseURL, mux)))
}

// --- Auth handlers ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	// Always return 200 regardless of whether the account exists.
	// Leaking existence is an enumeration vector.
	// Only an existing account gets a login link. A pending invitee has
	// their invite link, a prospective customer signs up; a login link is
	// neither of those.
	if user, _ := s.store.GetUserByEmail(req.Email); user == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	token, err := s.store.CreateMagicLink(req.Email)
	if err != nil {
		weftsdk.Infof(r.Context(), "login: create magic link for %s: %v", req.Email, err)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	verifyURL := fmt.Sprintf("%s/verify?token=%s", s.baseURL, token)
	if err := SendMagicLinkEmail(r.Context(), s.email, req.Email, verifyURL); err != nil {
		weftsdk.Infof(r.Context(), "login: send magic link to %s: %v", req.Email, err)
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string `json:"email"`
		OrgName string `json:"org_name"`
		Tier    string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	email := store.NormalizeEmail(req.Email)
	orgName := store.NormalizeOrgName(req.OrgName)
	if email == "" || orgName == "" || req.Tier == "" {
		http.Error(w, "email, org_name, and tier are required", http.StatusBadRequest)
		return
	}

	// Bolt is quoted per customer, so there is no public checkout for it.
	if _, ok := s.StripePrices[store.Tier(req.Tier)]; !ok || store.Tier(req.Tier) == store.TierBolt {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	// These checks are for the form's benefit; the verify step enforces
	// both again, since nothing is reserved in between.
	if existing, _ := s.store.GetUserByEmail(email); existing != nil {
		http.Error(w, "account already exists", http.StatusConflict)
		return
	}
	if existing, _ := s.store.GetOrgByName(orgName); existing != nil {
		http.Error(w, "organization name already taken", http.StatusConflict)
		return
	}

	// Signup creates nothing durable and returns no session: the address is
	// unproven. The pending org name and tier ride on the emailed link, and
	// following it (handleVerify) is what creates the account.
	token, err := s.store.IssueMagicLink(&store.MagicLink{
		Email:   email,
		Purpose: store.MagicLinkSignup,
		OrgName: orgName,
		Tier:    req.Tier,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s", s.baseURL, token)
	if err := SendMagicLinkEmail(r.Context(), s.email, email, verifyURL); err != nil {
		weftsdk.Errorf(r.Context(), "signup: failed to send verification email to %s: %v", email, err)
		http.Error(w, "failed to send email", http.StatusBadGateway)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status": "verification_sent",
		"email":  email,
		"tier":   req.Tier,
	})
}

// handleResendVerification re-sends the verification email for the
// authenticated caller. No-op (returns 200) for users already verified so
// the frontend can call it unconditionally from a "resend" button without
// worrying about state. The email send itself is best-effort — we log but
// don't fail if Sendgrid rejects.
func (s *Server) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	user, err := s.store.GetUserByID(session.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if user.EmailVerifiedAt != nil {
		json.NewEncoder(w).Encode(map[string]any{"status": "already_verified"})
		return
	}

	token, err := s.store.CreateMagicLink(user.Email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s", s.baseURL, token)
	if err := SendMagicLinkEmail(r.Context(), s.email, user.Email, verifyURL); err != nil {
		weftsdk.Errorf(r.Context(), "resend-verification: failed to send to %s: %v", user.Email, err)
		http.Error(w, "failed to send email", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "sent"})
}

// handleWelcomeDismiss flips the caller's HasSeenWelcome flag to true.
// Idempotent — re-dismiss returns 200 without touching UpdatedAt. The
// frontend calls this when the welcome modal closes; subsequent logins
// skip the modal because /api/auth/me reports has_seen_welcome: true.
func (s *Server) handleWelcomeDismiss(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if err := s.store.DB().Model(&store.User{}).
		Where("id = ? AND has_seen_welcome = FALSE", session.UserID).
		Updates(map[string]any{
			"has_seen_welcome": true,
			"updated_at":       time.Now(),
		}).Error; err != nil {
		weftsdk.Infof(r.Context(), "welcome-dismiss: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

// handleVerify redeems a login or signup link. Which one it is comes from
// the link, never from what rows exist: a login link signs in an existing
// account, a signup link creates the account it was issued for. Invite links
// belong to handleAcceptInvite and are refused here.
//
// Redemption is one transaction that starts by consuming the link, so a
// failure at any step leaves nothing behind and the link still usable.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	var (
		user       *store.User
		created    bool
		signupTier string
	)
	err := s.store.InTx(func(tx *store.Store) error {
		link, err := tx.ConsumeMagicLink(req.Token)
		if err != nil {
			return err
		}
		switch link.Purpose {
		case store.MagicLinkLogin:
			user, err = tx.GetUserByEmail(link.Email)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return store.ErrMagicLinkInvalid
			}
			if err != nil {
				return err
			}
			return tx.VerifyUserEmail(user)
		case store.MagicLinkSignup:
			signupTier = link.Tier
			user, created, err = completeSignup(tx, link)
			return err
		default:
			return store.ErrMagicLinkInvalid
		}
	})
	if err != nil {
		s.writeLinkError(w, r, "verify", err)
		return
	}

	if created {
		if err := s.grantLoomMembership(r.Context(), user.ID, user.Email); err != nil {
			weftsdk.Infof(r.Context(), "verify: grant Loom membership for %s: %v", user.Email, err)
		}
	}
	s.writeSession(w, r, user, map[string]any{"signup_tier": signupTier})
}

// completeSignup creates what a signup link promised: a verified owner of a
// brand-new org, awaiting a card for the plan they chose. If the address already has an account (say it
// accepted an invite since signing up) the link only proves the address.
func completeSignup(tx *store.Store, link *store.MagicLink) (user *store.User, created bool, err error) {
	user, err = tx.GetUserByEmail(link.Email)
	if err == nil {
		return user, false, tx.VerifyUserEmail(user)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	org, err := tx.CreateOrg(link.OrgName)
	if err != nil {
		return nil, false, err
	}
	if user, err = tx.CreateVerifiedUser(link.Email); err != nil {
		return nil, false, err
	}
	// The card comes first. The org gets no active subscription, and so no
	// ingest, queries or shell, until Stripe holds a payment method for the
	// plan the visitor chose; the setup-intent step attaches the Stripe
	// subscription and the webhook activates this row.
	if err := tx.CreateSubscription(&store.Subscription{
		ID:       uuid.New().String(),
		OrgID:    org.ID,
		Tier:     link.Tier,
		Status:   "pending_payment",
		StartsAt: time.Now(),
	}); err != nil {
		return nil, false, err
	}
	// The owner membership is what binds user to org.
	if err := tx.AddTeamMemberWithEmail(org.ID, user.ID, link.Email, store.RoleOwner); err != nil {
		return nil, false, err
	}
	if err := tx.UpdateUserLastActiveOrg(user.ID, org.ID); err != nil {
		return nil, false, err
	}
	return user, true, nil
}

// writeLinkError maps a failed link redemption to a response. Every way a
// link can be bad reads the same; only a taken org name is told apart, since
// the caller can act on it.
func (s *Server) writeLinkError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, store.ErrMagicLinkInvalid):
		http.Error(w, store.ErrMagicLinkInvalid.Error(), http.StatusUnauthorized)
	case errors.Is(err, store.ErrOrgNameTaken):
		http.Error(w, "organization name already taken", http.StatusConflict)
	default:
		weftsdk.Errorf(r.Context(), "%s: %v", op, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// writeSession answers a successful redemption with a JWT and the same user
// shape /me returns — AuthContext seeds its user straight from this payload,
// and the welcome modal keys off onboarding_step and the orgs list it
// carries. extra is merged into the top level.
func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, user *store.User, extra map[string]any) {
	jwt, err := auth.Generate(user, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Reload so the response reflects the writes above (verified flag,
	// membership).
	if fresh, err := s.store.GetUserByID(user.ID); err == nil {
		user = fresh
	}
	activeID, activeName := s.resolveActiveOrg(user)
	resp := map[string]any{
		"token": jwt,
		"user":  s.userResponse(r.Context(), user, activeID, activeName),
	}
	for k, v := range extra {
		resp[k] = v
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	user, err := s.store.GetUserByID(session.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// The tour's last step asks the user to pick their own org in the
	// selector. Choosing one is the graduation: remove Loom here, on the
	// first request scoped elsewhere, rather than trusting the browser.
	if user.OnboardingStep == "graduate" && session.ActiveCustomerName != LoomCustomerSlug {
		if advanced, err := s.store.AdvanceOnboardingStep(user.ID, "graduate", "done"); err != nil {
			weftsdk.Infof(r.Context(), "onboarding: graduate %s on org switch: %v", user.ID, err)
		} else if advanced {
			if err := s.graduateOnboarding(r.Context(), user.ID); err != nil {
				weftsdk.Infof(r.Context(), "onboarding: graduate %s: %v", user.ID, err)
			}
			// graduateOnboarding pins the home org; the user chose this one.
			_ = s.store.UpdateUserLastActiveOrg(user.ID, session.ActiveOrgID)
			if fresh, err := s.store.GetUserByID(user.ID); err == nil {
				user = fresh
			}
		}
	}

	json.NewEncoder(w).Encode(s.userResponse(r.Context(), user, session.ActiveOrgID, session.ActiveCustomerName))
}

// userResponse builds the canonical user object the SPA consumes. Both
// /api/auth/me (on reload) and /api/auth/verify (at login) return it, so
// AuthContext gets the same shape whichever it hits first. Keeping it in
// one place is the fix for a class of bug where the two disagreed: /verify
// used to return a 4-field subset with no onboarding_step, so the welcome
// modal — which gates on that field off the object AuthContext seeds from
// /verify — never rendered on a fresh comp account's first session.
//
// activeOrgID/Name are the org the response is scoped to: the
// middleware-resolved session org for /me, or the user's landing org for
// /verify. Tier and the orgs "active" flag are computed against it.
func (s *Server) userResponse(ctx context.Context, user *store.User, activeOrgID store.OrgID, activeCustomerName string) map[string]any {
	// Tier and customer come from the resolved active org, not from a
	// user.CustomerName field that no longer exists.
	tier := store.Tier("none")
	pendingPaymentTier := ""
	if activeOrgID != 0 {
		if sub, _ := s.store.ActiveSubscription(activeOrgID); sub != nil {
			tier = store.Tier(sub.Tier)
		} else if pending, _ := s.store.PendingPaymentSubscription(activeOrgID); pending != nil {
			pendingPaymentTier = pending.Tier
		}
	}

	// Memberships drive the org switcher in the UI — return them so the
	// frontend can render a list without a second round-trip.
	userOrgs, _ := s.store.ListUserOrgs(user.ID)
	orgs := make([]map[string]any, 0, len(userOrgs))
	for _, m := range userOrgs {
		orgs = append(orgs, map[string]any{
			"id":     m.ID,
			"name":   m.Name,
			"role":   m.Role,
			"active": m.ID == activeOrgID.String(),
		})
	}

	// pending_graduation_notice is the success banner the home org shows
	// after a Loom tour completion. It tracks the queued notice, so it
	// goes false once the notice has been delivered anywhere — including
	// in the shell, which drains the same row.
	pendingGraduation := len(s.pendingGraduationIDs(ctx, user.ID)) > 0

	resp := map[string]any{
		"id":                   user.ID,
		"email":                user.Email,
		"name":                 user.Name,
		"tier":                 tier,
		"pending_payment_tier": pendingPaymentTier,
		"customer":             activeCustomerName,
		"orgs":                 orgs,
		"email_verified":       user.EmailVerifiedAt != nil,
		"has_seen_welcome":     user.HasSeenWelcome,
		"onboarding_step":      user.OnboardingStep,
		"onboarding_dismissed": user.OnboardingDismissed,
		// onboarding_prompt is the lesson copy for the current
		// shell-* step. The frontend renders it verbatim — that
		// way the lesson list lives in one place (the Go package)
		// and the UI doesn't need updating each time we add a
		// lesson. onboarding_action is the suggested command; the
		// web shell injects it into the input so the user can just
		// hit enter.
		"is_superadmin":             user.IsSuperadmin,
		"onboarding_prompt":         onboarding.PromptFor(user.OnboardingStep),
		"onboarding_action":         onboarding.ActionFor(user.OnboardingStep),
		"pending_graduation_notice": pendingGraduation,
	}
	if user.EmailVerifiedAt == nil {
		resp["verification_deadline"] = user.CreatedAt.Add(auth.UnverifiedGracePeriod).Unix()
	}
	return resp
}

// resolveActiveOrg picks the org a just-verified user lands in.
// Verify runs before AuthMiddleware establishes a session, so there's no
// resolved active org yet — fall back to the user's last_active_customer
// (set by every account-creation path), then to their oldest membership.
func (s *Server) resolveActiveOrg(user *store.User) (store.OrgID, string) {
	if user.LastActiveOrgID != nil {
		if c, err := s.store.GetOrgByID(*user.LastActiveOrgID); err == nil && c != nil {
			return c.ID, c.Name
		}
	}
	if mems, _ := s.store.ListUserOrgs(user.ID); len(mems) > 0 {
		if id, err := store.ParseOrgID(mems[0].ID); err == nil {
			return id, mems[0].Name
		}
	}
	return 0, ""
}

// --- Billing handlers ---

// ensureStripeCustomer returns the Stripe customer ID for a user, creating one
// on Stripe if the user doesn't have one yet.
func (s *Server) ensureStripeCustomer(ctx context.Context, u *store.User, customerName string) (string, error) {
	if u.StripeCustomerID != "" {
		return u.StripeCustomerID, nil
	}
	cust, err := customer.New(&stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
		Email:  stripe.String(u.Email),
		Metadata: map[string]string{
			"user_id":  u.ID,
			"customer": customerName,
		},
	})
	if err != nil {
		return "", err
	}
	if err := s.store.DB().Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{
		"stripe_customer_id": cust.ID,
		"updated_at":         time.Now(),
	}).Error; err != nil {
		return "", err
	}
	return cust.ID, nil
}

// handleSetupIntent creates (or reuses) a Stripe Subscription in trialing
// state with payment_behavior=default_incomplete and returns the
// pending_setup_intent's client_secret. The frontend mounts Elements against
// that client_secret and calls stripe.confirmSetup() to collect card details.
func (s *Server) handleSetupIntent(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if s.authorize(r.Context(), session, hosted.ActionManageBilling, tenancy.Resource{}) != nil {
		http.Error(w, "only the owner can change the plan", http.StatusForbidden)
		return
	}

	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "tier required", http.StatusBadRequest)
		return
	}
	prices, ok := s.StripePrices[store.Tier(req.Tier)]
	if !ok {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	user, err := s.store.GetUserByID(session.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	stripe.Key = s.StripeSecretKey

	custID, err := s.ensureStripeCustomer(r.Context(), user, session.ActiveCustomerName)
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe customer create failed: %v", err)
		http.Error(w, "billing setup failed", http.StatusInternalServerError)
		return
	}

	// The row this checkout belongs to: the pending one a signup leaves, or
	// the active legacy trial of an account that predates card-first signup.
	trial, _ := s.store.PendingPaymentSubscription(session.ActiveOrgID)
	if trial == nil {
		trial, _ = s.store.ActiveSubscription(session.ActiveOrgID)
	}

	// Reuse an existing trialing Stripe subscription if the trial row already
	// has one attached (handles page refresh / retries idempotently).
	if trial != nil && trial.StripeSubscriptionID != "" {
		getParams := &stripe.SubscriptionParams{Params: stripe.Params{Context: r.Context()}}
		getParams.AddExpand("pending_setup_intent")
		sub, err := subscription.Get(trial.StripeSubscriptionID, getParams)
		if err == nil && sub.PendingSetupIntent != nil && sub.PendingSetupIntent.ClientSecret != "" {
			json.NewEncoder(w).Encode(map[string]string{
				"client_secret":   sub.PendingSetupIntent.ClientSecret,
				"publishable_key": s.StripePublishableKey,
			})
			return
		}
	}

	// Flat-rate item always; metered overage item only when the tier
	// has an overage price configured (absence = flat-only, useful
	// during a per-tier pilot of metered billing).
	items := []*stripe.SubscriptionItemsParams{
		{Price: stripe.String(prices.Flat), Quantity: stripe.Int64(1)},
	}
	if prices.Overage != "" {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(prices.Overage),
		})
	}
	params := &stripe.SubscriptionParams{
		Params:          stripe.Params{Context: r.Context()},
		Customer:        stripe.String(custID),
		Items:           items,
		TrialPeriodDays: stripe.Int64(14),
		PaymentBehavior: stripe.String("default_incomplete"),
		PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
			SaveDefaultPaymentMethod: stripe.String(string(stripe.SubscriptionPaymentSettingsSaveDefaultPaymentMethodOnSubscription)),
		},
		TrialSettings: &stripe.SubscriptionTrialSettingsParams{
			EndBehavior: &stripe.SubscriptionTrialSettingsEndBehaviorParams{
				MissingPaymentMethod: stripe.String(string(stripe.SubscriptionTrialSettingsEndBehaviorMissingPaymentMethodCancel)),
			},
		},
		Metadata: map[string]string{
			"user_id":  user.ID,
			"customer": session.ActiveCustomerName,
			"tier":     req.Tier,
		},
	}
	params.AddExpand("pending_setup_intent")

	sub, err := subscription.New(params)
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe subscription create failed: %v", err)
		http.Error(w, "billing setup failed", http.StatusInternalServerError)
		return
	}
	if sub.PendingSetupIntent == nil || sub.PendingSetupIntent.ClientSecret == "" {
		weftsdk.Infof(r.Context(), "stripe subscription missing pending_setup_intent: id=%s", sub.ID)
		http.Error(w, "billing setup failed", http.StatusInternalServerError)
		return
	}

	// Attach the Stripe subscription ID to our local trial row so refresh
	// reuses the same subscription and the webhook can promote it on confirm.
	if trial != nil {
		// Unscoped, but trial came from the org-scoped reads above,
		// so this id is already known to belong to the caller's org.
		_ = s.store.DB().WithContext(store.WithoutTenantScope(r.Context())).Model(&store.Subscription{}).
			Where("id = ?", trial.ID).
			Updates(map[string]any{
				"stripe_subscription_id": sub.ID,
				"updated_at":             time.Now(),
			}).Error
	}

	json.NewEncoder(w).Encode(map[string]string{
		"client_secret":   sub.PendingSetupIntent.ClientSecret,
		"publishable_key": s.StripePublishableKey,
	})
}

// handleCheckout handles tier changes for an existing paid subscription.
// It updates the Stripe subscription with the new price and returns either
// {status: "active"} if no confirmation is needed, or a client_secret for the
// latest invoice's PaymentIntent so the frontend can confirm inline.
func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if s.authorize(r.Context(), session, hosted.ActionManageBilling, tenancy.Resource{}) != nil {
		http.Error(w, "only the owner can change the plan", http.StatusForbidden)
		return
	}

	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "tier required", http.StatusBadRequest)
		return
	}
	newPrices, ok := s.StripePrices[store.Tier(req.Tier)]
	if !ok {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	stripe.Key = s.StripeSecretKey

	currentSub, _ := s.store.ActiveSubscription(session.ActiveOrgID)
	if currentSub == nil || currentSub.StripeSubscriptionID == "" {
		http.Error(w, "no active stripe subscription; complete signup first", http.StatusBadRequest)
		return
	}

	// Fetch the existing subscription so we can update items in place.
	// We identify each item by its price's recurring.usage_type:
	// "licensed" for the flat tier item, "metered" for the overage
	// item (if present). Matching by position would be fragile when
	// Stripe reorders or a new tier adds the overage item for the
	// first time.
	existing, err := subscription.Get(currentSub.StripeSubscriptionID, &stripe.SubscriptionParams{Params: stripe.Params{Context: r.Context()}})
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe subscription lookup failed: %v", err)
		http.Error(w, "billing update failed", http.StatusInternalServerError)
		return
	}
	if len(existing.Items.Data) == 0 {
		http.Error(w, "subscription has no items", http.StatusInternalServerError)
		return
	}
	var flatItemID, overageItemID string
	for _, it := range existing.Items.Data {
		if it.Price == nil || it.Price.Recurring == nil {
			continue
		}
		switch it.Price.Recurring.UsageType {
		case stripe.PriceRecurringUsageTypeMetered:
			overageItemID = it.ID
		default:
			flatItemID = it.ID
		}
	}
	if flatItemID == "" {
		// Shouldn't happen — every subscription has a flat item — but
		// fall back to the first item rather than silently misroute.
		flatItemID = existing.Items.Data[0].ID
	}

	// Update the flat item in place. Add or update the overage item
	// according to the new tier's config — absence means flat-only.
	items := []*stripe.SubscriptionItemsParams{
		{ID: stripe.String(flatItemID), Price: stripe.String(newPrices.Flat)},
	}
	if newPrices.Overage != "" {
		oi := &stripe.SubscriptionItemsParams{Price: stripe.String(newPrices.Overage)}
		if overageItemID != "" {
			oi.ID = stripe.String(overageItemID)
		}
		items = append(items, oi)
	} else if overageItemID != "" {
		// New tier has no overage item but old one does — delete it.
		items = append(items, &stripe.SubscriptionItemsParams{
			ID:      stripe.String(overageItemID),
			Deleted: stripe.Bool(true),
		})
	}

	updateParams := &stripe.SubscriptionParams{
		Params:            stripe.Params{Context: r.Context()},
		Items:             items,
		ProrationBehavior: stripe.String("create_prorations"),
		PaymentBehavior:   stripe.String("default_incomplete"),
		Metadata: map[string]string{
			"tier":     req.Tier,
			"user_id":  session.UserID,
			"customer": session.ActiveCustomerName,
		},
	}
	updateParams.AddExpand("latest_invoice.confirmation_secret")

	updated, err := subscription.Update(currentSub.StripeSubscriptionID, updateParams)
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe subscription update failed: %v", err)
		http.Error(w, "billing update failed", http.StatusInternalServerError)
		return
	}

	// If the invoice needs a confirmation (3DS or first charge), return its
	// client_secret so the frontend can call stripe.confirmPayment().
	if updated.LatestInvoice != nil && updated.LatestInvoice.ConfirmationSecret != nil {
		cs := updated.LatestInvoice.ConfirmationSecret.ClientSecret
		if cs != "" {
			json.NewEncoder(w).Encode(map[string]any{
				"requires_action": true,
				"client_secret":   cs,
				"publishable_key": s.StripePublishableKey,
			})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]any{"status": "active"})
}

// ourPriceIDs returns the set of Stripe price IDs that belong to weft, drawn
// from the configured per-tier flat + overage prices. The webhook endpoint
// shares a Stripe account with sibling products (e.g. vimgym), so every
// product's events fan out to it; an event is ours only if it references one
// of these prices.
func (s *Server) ourPriceIDs() map[string]bool {
	ids := make(map[string]bool, len(s.StripePrices)*2)
	for _, p := range s.StripePrices {
		if p.Flat != "" {
			ids[p.Flat] = true
		}
		if p.Overage != "" {
			ids[p.Overage] = true
		}
	}
	return ids
}

// subscriptionHasOurPrice reports whether any item on the subscription
// references a weft price. Subscription items are included in the payload.
func (s *Server) subscriptionHasOurPrice(sub *stripe.Subscription) bool {
	return stripekit.SubscriptionHasOurPrice(sub, s.ourPriceIDs())
}

// invoiceHasOurPrice reports whether any line item on the invoice references a
// weft price. Invoice line items are included in the webhook payload.
func (s *Server) invoiceHasOurPrice(inv *stripe.Invoice) bool {
	return stripekit.InvoiceHasOurPrice(inv, s.ourPriceIDs())
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, stripekit.MaxWebhookBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	event, err := stripekit.VerifyWebhook(body, r.Header.Get("Stripe-Signature"), s.StripeWebhookSecret)
	if err != nil {
		// VerifyWebhook fails for two unrelated reasons — a genuine bad
		// signature, or an authentic event whose API-version release train
		// doesn't match the pinned stripe-go. Name which one, so the log
		// points at the real fix instead of a phantom signing-secret problem.
		weftsdk.Infof(r.Context(), "Stripe webhook rejected: %s: %v", stripekit.ClassifyWebhookError(err), err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "customer.subscription.updated":
		// Covers trial→active and tier changes. Promote the local row to the
		// tier recorded in metadata, clearing any trial expiry.
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			weftsdk.Errorf(r.Context(), "Stripe webhook unmarshal error: %v", err)
			break
		}
		// Shared Stripe account: ignore sibling products' subscriptions.
		if !s.subscriptionHasOurPrice(&sub) {
			break
		}
		tier := sub.Metadata["tier"]
		// Only promote once the subscription is actually paying (not trialing
		// without a PM, and not past_due). A trialing subscription exists, and
		// gets an update, before any card is entered, so trialing counts only
		// once the card is saved as its default payment method.
		paying := sub.Status == stripe.SubscriptionStatusActive ||
			(sub.Status == stripe.SubscriptionStatusTrialing && sub.DefaultPaymentMethod != nil)
		if tier != "" && paying {
			// Unscoped: no session on a webhook. The signature authenticates the call
			// and the Stripe subscription id selects the row.
			if err := s.store.DB().WithContext(store.WithoutTenantScope(context.WithoutCancel(r.Context()))).Model(&store.Subscription{}).
				Where("stripe_subscription_id = ? AND comp_reason = ''", sub.ID).
				Updates(map[string]any{
					"tier":       tier,
					"status":     "active",
					"expires_at": nil,
					"updated_at": time.Now(),
				}).Error; err != nil {
				weftsdk.Errorf(r.Context(), "failed to promote subscription %s: %v", sub.ID, err)
			} else {
				weftsdk.Infof(r.Context(), "subscription promoted: stripe_sub=%s tier=%s status=%s", sub.ID, tier, sub.Status)
			}
		}

	case "invoice.payment_failed":
		// Mark the local subscription past_due so the UI can prompt for a new card.
		var inv stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
			weftsdk.Errorf(r.Context(), "Stripe webhook unmarshal error: %v", err)
			break
		}
		// Shared Stripe account: ignore sibling products' invoices.
		if !s.invoiceHasOurPrice(&inv) {
			break
		}
		if inv.Parent == nil || inv.Parent.SubscriptionDetails == nil {
			break
		}
		if inv.Parent.SubscriptionDetails.Subscription != nil {
			subID := inv.Parent.SubscriptionDetails.Subscription.ID
			// Unscoped: no session on a webhook. The signature authenticates the call
			// and the Stripe subscription id selects the row.
			_ = s.store.DB().WithContext(store.WithoutTenantScope(context.WithoutCancel(r.Context()))).Model(&store.Subscription{}).
				Where("stripe_subscription_id = ? AND comp_reason = ''", subID).
				Updates(map[string]any{
					"status":     "past_due",
					"updated_at": time.Now(),
				}).Error
			weftsdk.Errorf(r.Context(), "subscription payment failed: stripe_sub=%s", subID)
		}

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			weftsdk.Errorf(r.Context(), "Stripe webhook unmarshal error: %v", err)
			break
		}
		// Shared Stripe account: ignore sibling products' subscriptions.
		if !s.subscriptionHasOurPrice(&sub) {
			break
		}
		// Unscoped: no session on a webhook. The signature authenticates the call
		// and the Stripe subscription id selects the row.
		if err := s.store.DB().WithContext(store.WithoutTenantScope(context.WithoutCancel(r.Context()))).Model(&store.Subscription{}).
			Where("stripe_subscription_id = ? AND comp_reason = ''", sub.ID).
			Updates(map[string]any{
				"status":     "cancelled",
				"updated_at": time.Now(),
			}).Error; err != nil {
			weftsdk.Errorf(r.Context(), "failed to cancel subscription %s: %v", sub.ID, err)
		} else {
			weftsdk.Infof(r.Context(), "subscription cancelled: %s", sub.ID)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// projectedOverageCents returns what an extra $N the customer's next
// invoice would carry if the current period's ingestion stopped now.
// Cents, integer-truncated — the dashboard uses this as a heads-up,
// not an invoice line item (Stripe does the precise math at billing
// time). Zero when under the tier allowance or on tiers without an
// overage rate (trial).
func projectedOverageCents(bytes int64, tier store.Tier) int {
	over := bytes - store.VolumeAllowance(tier)
	if over <= 0 {
		return 0
	}
	rate := store.OverageRatePerGB(tier) // cents per decimal GB
	if rate == 0 {
		return 0
	}
	return int((over * int64(rate)) / 1_000_000_000)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if sub, _ := s.store.ActiveSubscription(session.ActiveOrgID); sub == nil {
		http.Error(w, "no active subscription", http.StatusPaymentRequired)
		return
	}

	resp, err := s.coordClient.GetUsage(s.coordCtx(r), &pb.GetUsageRequest{})
	if err != nil {
		weftsdk.Errorf(r.Context(), "GetUsage gRPC error: %v", err)
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}

	// Tier from the resolved Session (which BuildSession looks up
	// via TierForOrg); fall through to the active subscription
	// in case display tier evolves separately.
	tier := session.Tier
	if sub, _ := s.store.ActiveSubscription(session.ActiveOrgID); sub != nil {
		tier = store.Tier(sub.Tier)
	}

	result := map[string]any{
		"bytes_ingested":          resp.BytesIngested,
		"tier_limit_bytes":        store.VolumeAllowance(tier),
		"period_start":            resp.PeriodStart,
		"period_end":              resp.PeriodEnd,
		"tier":                    tier,
		"projected_overage_cents": projectedOverageCents(resp.BytesIngested, tier),
	}

	// Add metrics forecast if available.
	if cust, err := s.store.GetOrgByName(session.ActiveCustomerName); err == nil {
		if forecast, err := s.store.ForecastMetricsUsage(cust.ID); err == nil && forecast.AgentsWithMetrics > 0 {
			result["metrics_forecast"] = map[string]any{
				"agents_with_metrics":   forecast.AgentsWithMetrics,
				"projected_bytes_month": forecast.ProjectedBytesMonth,
			}
		}
	}

	json.NewEncoder(w).Encode(result)
}

// --- Saved queries handlers ---

// --- Token management handlers ---

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}

	tokens, err := s.store.ListAgentTokens(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The raw token is never returned from this endpoint — only on
	// the one-time create response. Callers identify tokens by their
	// opaque public id; the primary key and the raw-token prefix stay
	// server-side.
	type tokenResult struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		Namespace string `json:"namespace"`
		Env       string `json:"env"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]tokenResult, 0, len(tokens))
	for _, t := range tokens {
		status := "active"
		if t.Revoked {
			status = "revoked"
		}
		tr := tokenResult{
			ID:        t.PublicID,
			Label:     t.Label,
			Namespace: t.Namespace.Name,
			Env:       t.Env,
			Status:    status,
			CreatedAt: t.CreatedAt.Format(time.RFC3339),
		}
		out = append(out, tr)
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}

	var req struct {
		Label     string `json:"label"`
		Namespace string `json:"namespace"`
		Env       string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}

	// A token is scoped to one existing namespace. Looking it up (never
	// creating it) means a typo or a comma-separated list can't silently
	// mint a token for a brand-new namespace; coordinator CreateToken
	// (gRPC) holds the same rule.
	nsObj, err := s.store.GetNamespace(custID, req.Namespace)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if verr := store.ValidateNamespaceName(req.Namespace); verr != nil {
			http.Error(w, fmt.Sprintf("a token is scoped to one namespace: %v", verr), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("namespace %q not found — create it first", req.Namespace), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.authorize(r.Context(), session, tenancy.ActionManageNamespaceTokens, tenancy.Resource{Namespace: req.Namespace}); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	raw, tok, err := s.store.CreateAgentToken(custID, nsObj.ID, req.Label, req.Env)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Raw token is returned here and nowhere else — the UI must
	// capture it in this response or the user creates a new one.
	resp := map[string]any{
		"token":      raw,
		"id":         tok.PublicID,
		"label":      tok.Label,
		"namespace":  nsObj.Name,
		"env":        tok.Env,
		"status":     "active",
		"created_at": tok.CreatedAt.Format(time.RFC3339),
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	err := s.store.RevokeAgentToken(custID, id)
	if errors.Is(err, store.ErrTokenNotFound) {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Apps ---

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	resp, err := s.coordClient.ListServices(s.coordCtx(r), &pb.ListServicesRequest{})
	if err != nil {
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(resp.Services)
}

func (s *Server) handleIngestionStatus(w http.ResponseWriter, r *http.Request) {
	var apps []string
	err := s.store.DB().Raw(`
		SELECT DISTINCT unnest(apps) AS app
		FROM ingestion_errors
		WHERE resolved_at IS NULL
		AND occurred_at > NOW() - interval '1 hour'
		ORDER BY app`).Scan(&apps).Error
	if err != nil {
		apps = []string{}
	}
	json.NewEncoder(w).Encode(map[string]any{"affected_services": apps})
}

// --- Shell functions (/bin/) ---

// activeOrg returns the org id the session is scoped to. A session with no
// active org (a superadmin who belongs to none) has no org-scoped data, so
// that is a 403 rather than a lookup that would create an org on the fly.
func activeOrg(w http.ResponseWriter, session *auth.Session) (store.OrgID, bool) {
	if session.ActiveOrgID == 0 {
		http.Error(w, "no active org", http.StatusForbidden)
		return 0, false
	}
	return session.ActiveOrgID, true
}

func (s *Server) handleListFunctions(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	fns, err := s.store.ListShellFunctions(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"functions": fns,
	})
}

func (s *Server) handleGetFunction(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	fn, err := s.store.GetShellFunction(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(fn.Source))
}

func (s *Server) handleSaveFunction(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	var req struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Source == "" {
		http.Error(w, "source required", http.StatusBadRequest)
		return
	}
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	p, ok := mountPath("/bin", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).WriteFile(p, []byte(req.Source)); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteFunction(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	p, ok := mountPath("/bin", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).Remove(p); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Cron watches (/cron.d/) ---

func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watches, err := s.store.ListCronWatches(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tier := store.TierTrial
	if sub, _ := s.store.ActiveSubscription(session.ActiveOrgID); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	count, _ := s.store.CountCronWatches(custID)
	json.NewEncoder(w).Encode(map[string]any{
		"crons":        watches,
		"count":        count,
		"limit":        store.WatchLimit(tier),
		"min_interval": store.MinWatchInterval(tier).String(),
	})
}

func (s *Server) handleGetWatch(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	w_, err := s.store.GetCronWatch(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(w_)
}

func (s *Server) handleSaveWatch(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	var req struct {
		Schedule string `json:"schedule"`
		Pipeline string `json:"pipeline"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Schedule == "" || req.Pipeline == "" {
		http.Error(w, "schedule and pipeline required", http.StatusBadRequest)
		return
	}
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	p, ok := mountPath("/cron.d", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}

	// A save through the API always leaves the cron enabled. The group
	// check, plan limits, schedule validation and save-time lowering are the
	// /cron.d mount's, shared with the shell. WriteFile's error may be a
	// non-blocking store.Warning (the save already happened but there is
	// something to tell the user, e.g. the truncation lint); store.Split
	// separates that from a real rejection.
	content := "# schedule: " + req.Schedule + "\n" + req.Pipeline
	blocking, warnings := store.Split(s.shellFS(r, session, nil).WriteFile(p, []byte(content)))
	if len(blocking) > 0 {
		writeFSError(w, errors.Join(blocking...))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Warnings []string `json:"warnings,omitempty"`
	}{Warnings: warnings})
}

// storeBinResolver adapts a *store.Store + customer scope to the
// planner.BinResolver interface. Save handlers wrap their store in
// this adapter so Lower can read /bin/ source for inlining. Returns
// ok=false for any error path (missing /bin, DB miss) — Lower then
// leaves the /bin/ token unexpanded and the lowering may surface the
// resulting unplannable shape as a Lower error, in which case
// LowerForStorage logs and returns nil.
type storeBinResolver struct {
	store *store.Store
	orgID store.OrgID
}

func (r storeBinResolver) Source(name string) (string, string, bool) {
	fn, err := r.store.GetShellFunction(r.orgID, name)
	if err != nil || fn == nil {
		return "", "", false
	}
	return fn.Source, planner.HashBinSource(fn.Source), true
}

func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	p, ok := mountPath("/cron.d", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).Remove(p); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWatchResults(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watch, err := s.store.GetCronWatch(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	results, err := s.store.ListCronResults(watch.ID, 50, "")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(results)
}

// handleWatchLatest implements GET /api/crons/{name}/latest: the points of
// the cron's latest run, one per tag in table order, for the terminal
// dashboard's table and counter tiles.
func (s *Server) handleWatchLatest(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watch, err := s.store.GetCronWatch(custID, r.PathValue("name"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	points, err := s.latestPoints(watch.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(points)
}

// latestPoints is a cron's latest run as chart.LatestPoints.
func (s *Server) latestPoints(cronWatchID uint) ([]chart.LatestPoint, error) {
	rows, err := s.store.LatestCronRun(cronWatchID)
	if err != nil {
		return nil, err
	}
	points := make([]chart.LatestPoint, len(rows))
	for i, r := range rows {
		points[i] = chart.LatestPoint{Tag: r.Tag, Value: r.Value, Text: r.TextValue}
	}
	return points, nil
}

// rangeDurations maps the discrete range tokens accepted by the
// chart endpoint to their wall-clock windows. Keep in sync with the
// frontend's Watch page buttons — the server is the source of truth
// for which ranges exist; the client renders a button per entry.
var rangeDurations = map[string]time.Duration{
	"1h":  1 * time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
}

// handleWatchChart powers the /watch/:name detail page. Returns one
// point per (tag, bucket) over the requested range at the coarsest
// granularity whose retention covers it. Rollup tiers carry count,
// min, and max per bucket so the client can shade a band alongside
// the average line.
func (s *Server) handleWatchChart(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	// The window is either one of the Watch page's range buttons or, from
	// the shell's `chart <cron>`, the shell's own range spec, resolved here
	// against the server's clock like every other query.
	rangeKey := r.URL.Query().Get("range")
	var start, end time.Time
	if spec := r.URL.Query().Get("spec"); spec != "" {
		var err error
		if start, end, err = timerange.ParseRange(spec); err != nil {
			http.Error(w, "range: "+err.Error(), http.StatusBadRequest)
			return
		}
		rangeKey = spec
	} else {
		if rangeKey == "" {
			rangeKey = "24h"
		}
		dur, ok := rangeDurations[rangeKey]
		if !ok {
			http.Error(w, "unknown range", http.StatusBadRequest)
			return
		}
		end = time.Now()
		start = end.Add(-dur)
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watch, err := s.store.GetCronWatch(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if watch.ValueType == store.ValueText {
		http.Error(w, fmt.Sprintf("cron %q stores text (# type: text), so it has no numbers to chart; a dashboard table tile shows its latest values", name), http.StatusBadRequest)
		return
	}
	series, err := s.store.CronSeries(custID, watch.ID, start, end)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buckets := series.Buckets

	// Shape for the wire: flat points, avg computed here so the client
	// doesn't have to re-do sum/count division. Keep count/min/max so
	// legends can show per-tag stats without a second request.
	// Sum rides along so a reader can merge buckets exactly (avg of a
	// merged bucket is total sum / total count).
	type point struct {
		Ts    int64   `json:"ts"`
		Tag   string  `json:"tag"`
		Value float64 `json:"value"` // avg = sum/count (or raw value for raw tier)
		Count int64   `json:"count"`
		Sum   float64 `json:"sum"`
		Min   float64 `json:"min"`
		Max   float64 `json:"max"`
	}
	points := make([]point, 0, len(buckets))
	for _, b := range buckets {
		points = append(points, point{
			Ts:    b.BucketStart.UnixMilli(),
			Tag:   b.Tag,
			Value: b.Sum / float64(b.Count),
			Count: b.Count,
			Sum:   b.Sum,
			Min:   b.Min,
			Max:   b.Max,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{
		"points":      points,
		"granularity": series.Granularity,
		"range":       rangeKey,
		"start":       series.Start.UnixMilli(),
		"end":         series.End.UnixMilli(),
		"alert":       watch.Alert(),
	})
}

// --- Team management ---

func (s *Server) handleListTeam(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	members, err := s.store.ListTeamMembers(session.ActiveOrgID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	count, _ := s.store.CountTeamMembers(session.ActiveOrgID)
	tier := store.TierTrial
	if sub, _ := s.store.ActiveSubscription(session.ActiveOrgID); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	orgName := session.ActiveCustomerName
	if cust, err := s.store.GetOrgByName(session.ActiveCustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	json.NewEncoder(w).Encode(map[string]any{
		"members":     members,
		"seats_used":  count,
		"seats_limit": store.SeatLimit(tier),
		"org_name":    orgName,
	})
}

func (s *Server) handleInviteTeamMember(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	member, err := s.inviteMember(r.Context(), session, req.Email)
	if err != nil {
		if ie, ok := err.(*inviteErr); ok {
			http.Error(w, ie.msg, ie.status)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(member)
}

// handleAcceptInvite redeems an invite link: it joins the invitee to the one
// org the link names and nothing else. Same shape as handleVerify — one
// transaction that begins by consuming the link.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	var (
		user    *store.User
		created bool
	)
	err := s.store.InTx(func(tx *store.Store) error {
		link, err := tx.ConsumeMagicLink(req.Token)
		if err != nil {
			return err
		}
		if link.Purpose != store.MagicLinkInvite {
			return store.ErrMagicLinkInvalid
		}

		// Clicking the link proves the address, so the user is verified
		// whether they already had an account or not.
		user, err = tx.GetUserByEmail(link.Email)
		switch {
		case err == nil:
			if err := tx.VerifyUserEmail(user); err != nil {
				return err
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			if user, err = tx.CreateVerifiedUser(link.Email); err != nil {
				return err
			}
			created = true
		default:
			return err
		}

		member, err := tx.AcceptTeamMember(link.InviteID, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInviteGone
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(member.Email, link.Email) {
			return store.ErrMagicLinkInvalid
		}
		orgID, err := store.ParseOrgID(member.OrgID)
		if err != nil {
			return err
		}
		return tx.UpdateUserLastActiveOrg(user.ID, orgID)
	})
	if errors.Is(err, errInviteGone) {
		http.Error(w, "this invite is no longer valid", http.StatusNotFound)
		return
	}
	if err != nil {
		s.writeLinkError(w, r, "accept invite", err)
		return
	}

	if created {
		if err := s.grantLoomMembership(r.Context(), user.ID, user.Email); err != nil {
			weftsdk.Infof(r.Context(), "accept invite: grant Loom membership for %s: %v", user.Email, err)
		}
	}
	s.writeSession(w, r, user, nil)
}

// errInviteGone: the invite the link names was withdrawn (or never existed).
var errInviteGone = errors.New("invite no longer valid")

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	// Verify caller is owner.
	callerMember, err := s.store.GetTeamMemberByEmail(session.ActiveOrgID, session.Email)
	if err != nil || callerMember.Role != store.RoleOwner {
		http.Error(w, "only the owner can remove members", http.StatusForbidden)
		return
	}

	target, err := s.store.GetTeamMemberByID(session.ActiveOrgID, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.Error(w, "member not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if target.Email == session.Email {
		http.Error(w, "cannot remove yourself", http.StatusBadRequest)
		return
	}

	if err := s.store.RemoveMember(session.ActiveOrgID, target.Email); err != nil {
		if errors.Is(err, store.ErrOwnerImmutable) {
			http.Error(w, "owners cannot be removed", http.StatusForbidden)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Membership is removed; user record itself is unchanged (the user
	// may belong to other orgs).
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateTeamName(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	// Only the owner can rename.
	if callerMember, err := s.store.GetTeamMemberByEmail(session.ActiveOrgID, session.Email); err != nil || callerMember.Role != store.RoleOwner {
		http.Error(w, "only the owner can rename the team", http.StatusForbidden)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if err := s.store.UpdateOrgDisplayName(session.ActiveCustomerName, req.Name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"name": req.Name})
}

// ── Device auth flow (CLI login) ─────────────────────────────────────

// handleDeviceInit creates a new device code for CLI authorization.
// POST /api/auth/device → {"device_code": "...", "user_code": "XXXX-XX", "expires_in": 600}
func (s *Server) handleDeviceInit(w http.ResponseWriter, r *http.Request) {
	deviceBytes := make([]byte, 16)
	if _, err := rand.Read(deviceBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	userBytes := make([]byte, 3)
	if _, err := rand.Read(userBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dc := store.DeviceCode{
		DeviceCode: fmt.Sprintf("%x", deviceBytes),
		UserCode:   fmt.Sprintf("%X-%X", userBytes[:2], userBytes[2:]),
		ExpiresAt:  time.Now().Add(10 * time.Minute),
		CreatedAt:  time.Now(),
	}
	if err := s.store.DB().Create(&dc).Error; err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"device_code": dc.DeviceCode,
		"user_code":   dc.UserCode,
		"expires_in":  600,
	})
}

// handleDevicePoll checks if a device code has been approved.
// POST /api/auth/device/poll {"device_code": "..."}
// Returns {"status": "pending"} or {"status": "approved", "token": "jwt..."}
func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "device_code required", http.StatusBadRequest)
		return
	}

	var pollDC store.DeviceCode
	var userID string
	if err := s.store.DB().Where("device_code = ?", req.DeviceCode).First(&pollDC).Error; err != nil {
		http.Error(w, "invalid device code", http.StatusGone)
		return
	}
	if time.Now().After(pollDC.ExpiresAt) {
		http.Error(w, "device code expired", http.StatusGone)
		return
	}
	if pollDC.UserID != nil {
		userID = *pollDC.UserID
	}
	if userID == "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}

	user, err := s.store.GetUserByID(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	jwt, err := auth.Generate(user, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The CLI persists this so subsequent calls send X-Active-Org. Resolve
	// from last_active_org_id, falling back to the oldest membership.
	defaultOrg := ""
	if userOrgs, _ := s.store.ListUserOrgs(user.ID); len(userOrgs) > 0 {
		defaultOrg = userOrgs[0].Name
		if user.LastActiveOrgID != nil {
			for _, m := range userOrgs {
				if m.ID == user.LastActiveOrgID.String() {
					defaultOrg = m.Name
					break
				}
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":   "approved",
		"token":    jwt,
		"email":    user.Email,
		"customer": defaultOrg,
	})
}

// handleDeviceApprove approves a device code (requires auth — user clicks in browser).
// POST /api/auth/device/approve {"user_code": "XXXX-XX"}
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserCode == "" {
		http.Error(w, "user_code required", http.StatusBadRequest)
		return
	}

	result := s.store.DB().Model(&store.DeviceCode{}).
		Where("user_code = ? AND user_id IS NULL AND expires_at > ?", req.UserCode, time.Now()).
		Update("user_id", session.UserID)
	if result.RowsAffected == 0 {
		http.Error(w, "device code not found or expired", http.StatusNotFound)
		return
	}
	if result.Error != nil {
		http.Error(w, result.Error.Error(), http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
}

// ── Namespace management ──────────────────────────────────────

func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}

	nss, err := s.store.ListNamespaces(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	type nsResult struct {
		Name string `json:"name"`
		ID   uint   `json:"id"`
	}
	out := make([]nsResult, len(nss))
	for i, ns := range nss {
		out[i] = nsResult{Name: ns.Name, ID: ns.ID}
	}
	json.NewEncoder(w).Encode(map[string]any{"namespaces": out})
}

func (s *Server) handleCreateNamespace(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}

	if s.authorize(r.Context(), session, tenancy.ActionCreateNamespace, tenancy.Resource{}) != nil {
		http.Error(w, "forbidden: cannot create namespaces", http.StatusForbidden)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if err := store.ValidateNamespaceName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ns, err := s.store.FindOrCreateNamespace(custID, req.Name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Grant creator full access.
	s.store.GrantFullNamespaceAccess(session.UserID, ns.ID)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"name": ns.Name, "id": ns.ID})
}

// handleDeleteNamespace removes a namespace. Owner only. A namespace that
// still has sources or live tokens is refused with a 409 naming them, unless
// ?force=true, which removes it and everything keyed to it. Log data already
// stored in shards is not touched; it ages out under normal retention.
// DELETE /api/namespaces/{name}[?force=true]
func (s *Server) handleDeleteNamespace(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if s.authorize(r.Context(), session, tenancy.ActionDeleteNamespace, tenancy.Resource{}) != nil {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	name := r.PathValue("name")
	ns, err := s.store.GetNamespace(custID, name)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("force") != "true" {
		c, err := s.store.NamespaceContents(custID, ns.ID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if len(c.Sources) > 0 || c.Tokens > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error":   fmt.Sprintf("namespace %q is not empty", name),
				"sources": c.Sources,
				"tokens":  c.Tokens,
			})
			return
		}
	}

	if err := s.store.DeleteNamespace(r.Context(), custID, name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetNamespacePermission(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if s.authorize(r.Context(), session, hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}

	nsName := r.PathValue("name")
	userID := r.PathValue("userId")
	if nsName == "" || userID == "" {
		http.Error(w, "name and userId required", http.StatusBadRequest)
		return
	}

	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	ns, err := s.store.GetNamespace(custID, nsName)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	var req store.NamespacePermission
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.UserID = userID
	req.NamespaceID = ns.ID

	if err := s.store.UpsertNamespacePermission(&req); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleGetPermissions returns the active org's TeamPermsDoc — what
// every non-owner member can do across /bin /cron.d /etc and per-
// namespace. Readable by every authenticated org member (the same
// transparency as `cat /etc/passwd` in the shell). Backs the
// dashboard's Members grid.
func (s *Server) handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	doc, err := s.store.BuildTeamPerms(session.ActiveOrgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// handleSetPermissions backs the dashboard's Members grid — the JSON
// twin of the shell's adduser/deluser, but whole-doc rather than one
// grant at a time. Owner-only; validate-then-upsert. Validation errors
// come back as 400 with a readable message so the UI can surface them.
func (s *Server) handleSetPermissions(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if s.authorize(r.Context(), session, hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}
	var doc store.TeamPermsDoc
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := s.store.ApplyTeamPerms(session.ActiveOrgID, doc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Owner checks have moved to session.IsOwner — the active membership's
// role is materialized in BuildSession. Kept call sites that wanted a
// different scope (e.g. checking against a specific customer that isn't
// the active one) can call store.GetTeamMemberByEmail directly.
