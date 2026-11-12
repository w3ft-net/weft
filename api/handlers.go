package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/single"

	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/internal/timerange"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"gorm.io/gorm"
)

// unverifiedSweepInterval is how often the cleanup goroutine deletes
// expired unverified accounts. Must be comfortably shorter than
// UnverifiedGracePeriod so an expired account doesn't linger.
const unverifiedSweepInterval = 1 * time.Hour

// Server is the weft API server.
type Server struct {
	store      *store.Store
	tenancy    tenancy.Adapter    // who acts, what they may do (Authz) and read (Scope); the open one until SetTenancy
	alertKinds *alerting.Registry // the kinds a save of /etc/notify.yaml may name; nil is the core's
	identity   Identity           // authenticates the api's users; see SetIdentity
	coordAddr  string             // coordinator gRPC address
	baseURL    string             // e.g., https://w3ft.net

	// Coordinator gRPC client (connected lazily via Connect).
	coordConn   *grpc.ClientConn
	coordClient pb.WeftCoordinatorClient

	// extend adds a deployment's own routes to the mux; see Extend.
	extend []func(*Routes)
	// shellCommands are tried before a shell pipeline is forwarded; see AddShellCommand.
	shellCommands []ShellCommand

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

// SetTenancy installs the deployment's tenancy: how a session becomes an actor,
// what the actor may do and what it may read. Until it is called the server
// runs the open one, which refuses everything under a shape that has a tenant.
func (s *Server) SetTenancy(t tenancy.Adapter) {
	s.tenancy = t
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

// NewServer creates an API server. Wires a defaults-backed config
// pointer + limiter so callers that don't call SetConfig (tests,
// dev) still have a live limiter — production overwrites both via
// SetConfig before serving.
func NewServer(s *store.Store, coordAddr, baseURL string) *Server {
	cfgPtr := new(atomic.Pointer[config.Runtime])
	cfgPtr.Store(config.Defaults())
	return &Server{
		store:     s,
		tenancy:   single.Adapter(),
		coordAddr: coordAddr,
		baseURL:   baseURL,
		cfgPtr:    cfgPtr,
		limiter:   newLimiter(cfgPtr),
	}
}

// authorize asks the Authorizer whether the session's user may take act. Any
// error is a refusal.
// readable is the namespaces the session may read.
func (s *Server) readable(ctx context.Context, session Session) ([]string, error) {
	names, _, err := tenancy.Readable(ctx, s.tenancy.Scope, session.Actor(), func() ([]string, error) {
		return s.store.NamespaceNames(session.Org(), "", true)
	})
	return names, err
}

// sourceScope is what the session may read, held for the source endpoints. A
// source is a coordinate, not a namespace, so they are filtered by the scope
// itself and a scope narrower than a namespace is honoured.
type sourceScope struct {
	set   shape.Set
	whole shape.Coord // the actor's whole partition: every coordinate starts from it
}

func (s *Server) sourceScope(ctx context.Context, session Session) (sourceScope, error) {
	set, err := s.tenancy.Scope.ReadScope(ctx, session.Actor())
	return sourceScope{set: set, whole: session.Actor().Partition().Coord()}, err
}

// sees reports whether the scope reads any of the service's rows.
func (ss sourceScope) sees(namespace, service string) bool {
	c := ss.whole
	c.Namespace, c.Service = shape.Value(namespace), shape.Value(service)
	return ss.set.Overlaps(c)
}

// narrow limits a log-group query to the namespaces the scope touches. The
// caller still filters each row with sees.
func (ss sourceScope) narrow(q *gorm.DB) *gorm.DB {
	names, all := ss.set.NamespaceSuperset(ss.whole)
	if all {
		return q
	}
	return q.Where("namespaces.name IN ?", names)
}

func (s *Server) authorize(ctx context.Context, session Session, act tenancy.Action, res tenancy.Resource) error {
	return s.tenancy.Authz.Authorize(ctx, session.Actor(), act, res)
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
// behalf of the authenticated caller of r. What the Identity says the
// coordinator needs to authenticate the caller itself is forwarded as outgoing
// gRPC metadata.
func (s *Server) coordCtx(r *http.Request) context.Context {
	ctx := r.Context()
	for k, v := range s.ident().Forward(r) {
		ctx = metadata.AppendToOutgoingContext(ctx, k, v)
	}
	return ctx
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	cfg := s.currentConfig()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Authenticated routes.
	// protected wraps a handler in the Identity's gate for routes that touch
	// external effects (token creation, source registration, namespace
	// creation). Read-only routes stay open to a caller the gate would refuse.
	protected := func(h http.HandlerFunc) http.Handler {
		return s.ident().Protect(h)
	}

	auth := http.NewServeMux()
	auth.HandleFunc("GET /api/fs/list", s.handleListFS)
	auth.HandleFunc("GET /api/fs/read", s.handleReadFS)
	auth.HandleFunc("PUT /api/fs/write", s.handleWriteFS)
	auth.HandleFunc("DELETE /api/fs/remove", s.handleRemoveFS)
	auth.HandleFunc("GET /api/tokens", s.handleListTokens)
	auth.Handle("POST /api/tokens", protected(s.handleCreateToken))
	auth.Handle("DELETE /api/tokens/{id}", protected(s.handleRevokeToken))
	auth.HandleFunc("GET /api/apps", s.handleListApps)
	auth.HandleFunc("GET /api/ingestion-status", s.handleIngestionStatus)
	auth.Handle("POST /api/shell/exec", BodyCapFunc(cfg.ShellExecBodyMax, s.handleShellExec))
	auth.HandleFunc("GET /api/functions", s.handleListFunctions)
	auth.HandleFunc("GET /api/functions/{name}", s.handleGetFunction)
	auth.Handle("PUT /api/functions/{name}", BodyCap(cfg.FunctionBodyMax, protected(s.handleSaveFunction)))
	auth.Handle("DELETE /api/functions/{name}", protected(s.handleDeleteFunction))
	auth.HandleFunc("GET /api/crons", s.handleListWatches)
	// A cron is /api/crons/{name} at the org level, or /api/crons/{ns}/{name}
	// in a namespace; cronPath joins the two.
	for _, base := range []string{"/api/crons/{name}", "/api/crons/{ns}/{name}"} {
		auth.HandleFunc("GET "+base, s.handleGetWatch)
		auth.Handle("PUT "+base, BodyCap(cfg.WatchBodyMax, protected(s.handleSaveWatch)))
		auth.Handle("DELETE "+base, protected(s.handleDeleteWatch))
		auth.HandleFunc("GET "+base+"/results", s.handleWatchResults)
		auth.HandleFunc("GET "+base+"/latest", s.handleWatchLatest)
		auth.HandleFunc("GET "+base+"/chart", s.handleWatchChart)
	}
	auth.HandleFunc("GET /api/dashboards", s.handleListDashboards)
	auth.HandleFunc("GET /api/dashboards/{name}", s.handleGetDashboard)
	auth.Handle("PUT /api/dashboards/{name}", BodyCap(cfg.WatchBodyMax, protected(s.handleSaveDashboard)))
	auth.Handle("DELETE /api/dashboards/{name}", protected(s.handleDeleteDashboard))
	auth.HandleFunc("GET /api/notify", s.handleGetNotifyConfig)
	auth.Handle("PUT /api/notify", BodyCap(cfg.WatchBodyMax, protected(s.handleSaveNotifyConfig)))
	auth.Handle("PUT /api/integrations/webhook-secrets", protected(s.handleWebhookSecretSave))
	auth.Handle("DELETE /api/integrations/webhook-secrets/{name}", protected(s.handleWebhookSecretDelete))
	auth.HandleFunc("GET /api/namespaces", s.handleListNamespaces)
	auth.Handle("POST /api/namespaces", protected(s.handleCreateNamespace))
	auth.Handle("DELETE /api/namespaces/{name}", protected(s.handleDeleteNamespace))

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
	// Every path the core serves behind the session gate. Routes.Authed skips
	// the ones listed here, so a deployment can add a method to a path the core
	// already mounts.
	gate := s.ident().Authenticate(gatedAuth)
	mounts := map[string]bool{}
	for _, path := range []string{
		"/api/sources",
		"/api/sources/",
		"/api/tokens",
		"/api/tokens/",
		"/api/apps",
		"/api/shell/exec",
		"/api/functions",
		"/api/functions/",
		"/api/crons",
		"/api/crons/",
		"/api/dashboards",
		"/api/dashboards/",
		"/api/notify",
		"/api/integrations/webhook-secrets",
		"/api/integrations/webhook-secrets/",
		"/api/namespaces",
		"/api/namespaces/",
		"/api/fs/",
		"/api/ingestion-status",
	} {
		mux.Handle(path, gate)
		mounts[path] = true
	}

	// The identity's own routes (sign-in, tokens, the account), then the
	// deployment's (hosted: billing, admin, integrations).
	rt := &Routes{s: s, mux: mux, auth: auth, gated: gate, mounts: mounts}
	s.ident().Routes(rt)
	for _, fn := range s.extend {
		fn(rt)
	}

	return weftsdk.TraceMiddleware(AccessLog(CORSMiddleware(s.baseURL, mux)))
}

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
func activeOrg(w http.ResponseWriter, session Session) (store.OrgID, bool) {
	if session.Org() == 0 {
		http.Error(w, "no active org", http.StatusForbidden)
		return 0, false
	}
	return session.Org(), true
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

// cronPath is the cron a request names: {name} at the org level, {ns}/{name}
// in a namespace.
func cronPath(r *http.Request) string {
	if ns := r.PathValue("ns"); ns != "" {
		return ns + "/" + r.PathValue("name")
	}
	return r.PathValue("name")
}

// cronAt finds the cron at a /cron.d path, or reports it missing when it is
// homed in a namespace the caller can't read: the same answer as no such cron.
func (s *Server) cronAt(ctx context.Context, session Session, p string) (*store.CronWatch, error) {
	ns, name, err := s.store.ResolveCronPath(session.Org(), p)
	if err != nil {
		return nil, gorm.ErrRecordNotFound
	}
	if ns != 0 {
		readable, err := s.readable(ctx, session)
		if err != nil {
			return nil, err
		}
		if home, _, _ := strings.Cut(p, "/"); !slices.Contains(readable, home) {
			return nil, gorm.ErrRecordNotFound
		}
	}
	return s.store.GetCronWatch(session.Org(), ns, name)
}

// readableCrons lists the org-level crons and those homed in a namespace the
// caller can read.
func (s *Server) readableCrons(ctx context.Context, session Session) ([]store.CronWatch, error) {
	all, err := s.store.ListCronWatches(session.Org())
	if err != nil {
		return nil, err
	}
	readable, err := s.readable(ctx, session)
	if err != nil {
		return nil, err
	}
	var ids []uint
	for _, n := range readable {
		if ns, err := s.store.GetNamespace(session.Org(), n); err == nil {
			ids = append(ids, ns.ID)
		}
	}
	out := all[:0:0]
	for _, w := range all {
		if w.NamespaceID == nil || slices.Contains(ids, *w.NamespaceID) {
			out = append(out, w)
		}
	}
	return out, nil
}

func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watches, err := s.readableCrons(r.Context(), session)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tier := store.TierTrial
	if sub, _ := s.store.ActiveSubscription(session.Org()); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	count, _ := s.store.CountCronWatches(custID)
	// Each cron carries its path (<namespace>/<name>, or the bare name at the
	// org level), which is how every other call addresses it.
	nss, _ := s.store.ListNamespaces(custID)
	nsName := make(map[uint]string, len(nss))
	for _, n := range nss {
		nsName[n.ID] = n.Name
	}
	type cronItem struct {
		store.CronWatch
		Path string `json:"path"`
	}
	items := make([]cronItem, len(watches))
	for i, wt := range watches {
		items[i] = cronItem{CronWatch: wt, Path: wt.Name}
		if wt.NamespaceID != nil {
			items[i].Path = nsName[*wt.NamespaceID] + "/" + wt.Name
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"crons":        items,
		"count":        count,
		"limit":        store.WatchLimit(tier),
		"min_interval": store.MinWatchInterval(tier).String(),
	})
}

func (s *Server) handleGetWatch(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := cronPath(r)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	_, ok := activeOrg(w, session)
	if !ok {
		return
	}
	cw, err := s.cronAt(r.Context(), session, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(cw)
}

func (s *Server) handleSaveWatch(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := cronPath(r)
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
	name := cronPath(r)
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
	name := cronPath(r)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	_, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watch, err := s.cronAt(r.Context(), session, name)
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
	_, ok := activeOrg(w, session)
	if !ok {
		return
	}
	watch, err := s.cronAt(r.Context(), session, cronPath(r))
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
	name := cronPath(r)
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
	watch, err := s.cronAt(r.Context(), session, name)
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
	s.store.GrantFullNamespaceAccess(session.UserID(), ns.ID)

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

// Owner checks have moved to session.IsOwner — the active membership's
// role is materialized in BuildSession. Kept call sites that wanted a
// different scope (e.g. checking against a specific customer that isn't
// the active one) can call store.GetTeamMemberByEmail directly.
