package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	dashstore "github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/warning"

	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/identity"

	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/timerange"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/ingest"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// unverifiedSweepInterval is how often the cleanup goroutine deletes
// expired unverified accounts. Must be comfortably shorter than
// UnverifiedGracePeriod so an expired account doesn't linger.
const unverifiedSweepInterval = 1 * time.Hour

// Server is the weft API server.
type Server struct {
	tokens           token.Store  // agent tokens
	sources          source.Store // namespaces and sources
	bins             bin.Store    // /bin scripts
	namespaceCreated func(ctx context.Context, session Session, namespace uint)
	fleet            sources.AgentFleet     // who reads the agent-sampled sources; nil is none
	crons            cron.Store             // crons, their results and series
	dashboards       dashstore.Store        // /etc/dashboards; nil serves no dashboard routes
	limits           identity.Entitlements  // what each plan allows; nil is no limits
	ingest           ingestfail.Store       // ingestion failures
	notify           notifycfg.Service      // notification config and secrets
	group            func(shape.Added) bool // whether a partition names a group; nil: every one does
	mounts           *mounts.Backends       // what the shell's mounts read and write through
	producers        *sources.Registry      // the log producers; the HTTPS ones mount themselves in Handler
	kindsOnce        sync.Once
	kinds            *sources.Registry  // the rules of each kind of source: the core's, and the producers' own
	access           identity.Access    // who acts, what they may do (Authz) and read (Scope); the open one until SetAccess
	alertKinds       *alerting.Registry // the kinds a save of /etc/notify.yaml may name; nil is the core's
	identity         Identity           // authenticates the api's users; see SetIdentity
	coordAddr        string             // coordinator gRPC address
	baseURL          string             // the web app's external URL
	apiURL           string             // the api's own external URL

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

// SetTokens is where agent tokens live.
func (s *Server) SetTokens(t token.Store) { s.tokens = t }

// SetGroups says whether a partition names a group. A session whose partition
// does not (hosted: a superadmin who belongs to no org) has no group data, and
// the routes that read or write it refuse it. Without it every partition does.
func (s *Server) SetGroups(has func(shape.Added) bool) { s.group = has }

// SetNamespaceCreated installs what runs after a namespace is created over the
// api, with the session that made it (hosted: the creator is given full access
// to it).
func (s *Server) SetNamespaceCreated(fn func(ctx context.Context, session Session, namespace uint)) {
	s.namespaceCreated = fn
}

// SetSources is where namespaces and sources live.
func (s *Server) SetSources(st source.Store) { s.sources = st }

// SetNotify is where the notification config and webhook secrets live.
func (s *Server) SetNotify(svc notifycfg.Service) { s.notify = svc }

// NotifyService is the notification config and webhook secrets the server uses.
func (s *Server) NotifyService() notifycfg.Service { return s.notify }

// SetIngestFailures is where ingestion failures are recorded and read.
func (s *Server) SetIngestFailures(st ingestfail.Store) { s.ingest = st }

// SetBins is where /bin scripts live.
func (s *Server) SetBins(st bin.Store) { s.bins = st }

// SetMounts is what the shell's filesystem is built over (hosted: the Postgres
// registries, which hold each group to its own limits).
func (s *Server) SetMounts(b *mounts.Backends) { s.mounts = b }

// SetAgentFleet is where the agents reading each source are recorded. It must
// be set before the first request. Without it no source reports agents, and a
// schema is never refused for an agent's version.
func (s *Server) SetAgentFleet(f sources.AgentFleet) { s.fleet = f }

// SetCrons is where crons and their results live.
func (s *Server) SetCrons(st cron.Store) { s.crons = st }

// SetEntitlements is where each plan's limits come from. Without it the server
// applies none.
func (s *Server) SetEntitlements(e identity.Entitlements) { s.limits = e }

// limitsOf is the limits of the plan the session is on.
func (s *Server) limitsOf(session Session) identity.Limits {
	if s.limits == nil {
		return identity.Limits{}
	}
	return s.limits.LimitsFor(session.Principal().Partition())
}

// kindRules is the registry that says how each kind of source behaves (when it
// collects a sample, what a resample does): the core's kinds, and any producer
// the deployment registered under another name. A producer registered under a
// core name keeps the core's rules, which every push producer embeds.
func (s *Server) kindRules() *sources.Registry {
	s.kindsOnce.Do(func() {
		s.kinds = sources.Core(s.fleet)
		for _, name := range s.producers.Names() {
			if p, ok := s.producers.Get(name); ok {
				_ = s.kinds.Register(name, p) // a name the core has is already there
			}
		}
	})
	return s.kinds
}

// SetClassifier wires the LLM classifier used for pending-source schema
// detection. Optional — a nil/never-called classifier just means
// ClassifyAsync skips LLM classification (see NewBedrockClassifierFromEnv,
// which itself returns nil when unconfigured).
func (s *Server) SetClassifier(c ingest.Classifier) {
	s.classifier = c
}

// SetAccess installs the deployment's access: how a session becomes a principal,
// what the principal may do and what it may read. Until it is called the server
// runs the open one, which refuses everything under a shape that has a group.
func (s *Server) SetAccess(t identity.Access) {
	s.access = t
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

// apiBaseURL returns the API's own external URL, as set by SetAPIBaseURL.
func (s *Server) apiBaseURL() string { return s.apiURL }

// SetAPIBaseURL sets the API's own external URL (e.g. https://api.weft.example.com).
func (s *Server) SetAPIBaseURL(u string) { s.apiURL = u }

// NewServer creates an API server. Wires a defaults-backed config
// pointer + limiter so callers that don't call SetConfig (tests,
// dev) still have a live limiter — production overwrites both via
// SetConfig before serving.
func NewServer(coordAddr, baseURL string) *Server {
	cfgPtr := new(atomic.Pointer[config.Runtime])
	cfgPtr.Store(config.Defaults())
	return &Server{
		producers: DefaultProducers(),
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
	names, _, err := identity.Readable(ctx, s.access.Scope, session.Principal(), func() ([]string, error) {
		nss, err := s.sources.ListNamespaces(session.Principal().Partition())
		names := make([]string, len(nss))
		for i, n := range nss {
			names[i] = n.Name
		}
		slices.Sort(names)
		return names, err
	})
	return names, err
}

// sourceScope is what the session may read, held for the source endpoints. A
// source is a coordinate, not a namespace, so they are filtered by the scope
// itself and a scope narrower than a namespace is honoured.
type sourceScope struct {
	set   shape.Set
	whole shape.Coord // the principal's whole partition: every coordinate starts from it
}

func (s *Server) sourceScope(ctx context.Context, session Session) (sourceScope, error) {
	set, err := s.access.Scope.ReadScope(ctx, session.Principal())
	return sourceScope{set: set, whole: session.Principal().Partition().Coord()}, err
}

// sees reports whether the scope reads any of the service's rows.
func (ss sourceScope) sees(namespace, service string) bool {
	c := ss.whole
	c.Namespace, c.Service = shape.Value(namespace), shape.Value(service)
	return ss.set.Overlaps(c)
}

// filter limits a source listing to the namespaces the scope touches. The caller
// still filters each row with sees.
func (ss sourceScope) filter(f source.Filter) source.Filter {
	names, all := ss.set.NamespaceSuperset(ss.whole)
	if !all {
		if names == nil {
			names = []string{} // a scope that reads nothing, not every namespace
		}
		f.Namespaces = names
	}
	return f
}

func (s *Server) authorize(ctx context.Context, session Session, act identity.Action, res identity.Resource) error {
	return s.access.Authz.Authorize(ctx, session.Principal(), act, res)
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
	if s.dashboards != nil {
		auth.HandleFunc("GET /api/dashboards", s.handleListDashboards)
		auth.HandleFunc("GET /api/dashboards/{name}", s.handleGetDashboard)
		auth.Handle("PUT /api/dashboards/{name}", BodyCap(cfg.WatchBodyMax, protected(s.handleSaveDashboard)))
		auth.Handle("DELETE /api/dashboards/{name}", protected(s.handleDeleteDashboard))
	}
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
	auth.Handle("POST /api/sources", protected(s.handleAddSource))
	auth.Handle("DELETE /api/sources/{namespace}/{app}", protected(s.handleDeleteSource))

	// The producers that deliver over HTTPS mount themselves, each at the route
	// it names (Heroku drains, Firehose delivery from CloudWatch, OTLP metrics;
	// see the producer's own file). Whatever the
	// deployment registered beside them mounts the same way.
	sources.Each(s.producers, func(name string, p HTTPProducer) {
		mux.Handle(p.Route(), s.IngestHandler(p))
	})

	// Agent-facing routes (agent token auth).
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("POST /api/agent/sample", s.handleAgentSample)
	agentMux.HandleFunc("GET /api/agent/source-status", s.handleAgentSourceStatus)
	mux.Handle("/api/agent/", AgentTokenMiddleware(s.tokens, agentMux))

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
	gated := []string{
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
		"/api/notify",
		"/api/integrations/webhook-secrets",
		"/api/integrations/webhook-secrets/",
		"/api/namespaces",
		"/api/namespaces/",
		"/api/fs/",
		"/api/ingestion-status",
	}
	if s.dashboards != nil {
		gated = append(gated, "/api/dashboards", "/api/dashboards/")
	}
	for _, path := range gated {
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
	if !s.requireGroup(w, session) {
		return
	}

	tokens, err := s.tokens.ListTokens(session.Principal().Partition())
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
	if !s.requireGroup(w, session) {
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
	nsObj, found, err := s.sources.GetNamespace(session.Principal().Partition(), req.Namespace)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		if verr := source.ValidateNamespaceName(req.Namespace); verr != nil {
			http.Error(w, fmt.Sprintf("a token is scoped to one namespace: %v", verr), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("namespace %q not found — create it first", req.Namespace), http.StatusNotFound)
		return
	}

	if err := s.authorize(r.Context(), session, identity.ActionManageNamespaceTokens, identity.Resource{Namespace: req.Namespace}); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	raw, tok, err := s.tokens.CreateToken(session.Principal().Partition(), nsObj.ID, req.Label, req.Env)
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
	if !s.requireGroup(w, session) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	err := s.tokens.RevokeToken(session.Principal().Partition(), id)
	if errors.Is(err, token.ErrNotFound) {
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
	// Only the caller's own services: a failure is a group's, not the platform's.
	apps, err := s.ingest.AffectedServices(GetSession(r).Principal().Partition(), time.Now().Add(-time.Hour))
	if err != nil {
		apps = []string{}
	}
	json.NewEncoder(w).Encode(map[string]any{"affected_services": apps})
}

// --- Shell functions (/bin/) ---

// requireGroup is true when the session acts in a group. A session that acts
// in none (a superadmin who belongs to no org) has no group data, so that is a
// 403 rather than a lookup that would create one on the fly.
func (s *Server) requireGroup(w http.ResponseWriter, session Session) bool {
	if s.group != nil && !s.group(session.Principal().Partition()) {
		http.Error(w, "no active org", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleListFunctions(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if !s.requireGroup(w, session) {
		return
	}
	scripts, err := s.bins.List(session.Principal().Partition())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The fields the clients read; the owner stays out of the wire.
	type function struct {
		ID                     uint
		Name, Source, Language string
		CreatedAt, UpdatedAt   time.Time
	}
	fns := make([]function, len(scripts))
	for i, sc := range scripts {
		fns[i] = function{sc.ID, sc.Name, sc.Source, sc.Language, sc.CreatedAt, sc.UpdatedAt}
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
	if !s.requireGroup(w, session) {
		return
	}
	fn, err := s.bins.Get(session.Principal().Partition(), name)
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
	if !s.requireGroup(w, session) {
		return
	}
	p, ok := mountPath("/bin", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).WriteFile(r.Context(), p, []byte(req.Source)); err != nil {
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
	if !s.requireGroup(w, session) {
		return
	}
	p, ok := mountPath("/bin", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).Remove(r.Context(), p); err != nil {
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
func (s *Server) cronAt(ctx context.Context, session Session, p string) (*cron.Watch, error) {
	owner := session.Principal().Partition()
	if home, _, homed := strings.Cut(p, "/"); homed {
		readable, err := s.readable(ctx, session)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(readable, home) {
			return nil, cron.ErrNotFound
		}
	}
	return s.cronNamed(owner, p)
}

// readableCrons lists the org-level crons and those homed in a namespace the
// caller can read.
func (s *Server) readableCrons(ctx context.Context, session Session) ([]cron.Watch, error) {
	all, err := s.crons.ListWatches(session.Principal().Partition())
	if err != nil {
		return nil, err
	}
	readable, err := s.readable(ctx, session)
	if err != nil {
		return nil, err
	}
	var ids []uint
	for _, n := range readable {
		if ns, ok, err := s.sources.GetNamespace(session.Principal().Partition(), n); err == nil && ok {
			ids = append(ids, ns.ID)
		}
	}
	out := all[:0:0]
	for _, w := range all {
		if w.Namespace == 0 || slices.Contains(ids, w.Namespace) {
			out = append(out, w)
		}
	}
	return out, nil
}

// cronJSON is a cron on the wire. The keys are the ones the web app reads.
type cronJSON struct {
	ID                 uint
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Name               string
	NamespaceID        *uint
	Schedule           string
	Pipeline           string
	Enabled            bool
	ConsecutiveStrikes int
	LastRunAt          *time.Time
	LastStatus         string
	ValueField         *string
	TagField           *string
	HasAlert           bool
	ValueType          string
	AlertCondition     *string
	AlertThreshold     *float64
	ContentHash        string
	Message            string
}

func cronOf(w cron.Watch) cronJSON {
	out := cronJSON{
		ID: w.ID, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, Name: w.Name,
		Schedule: w.Schedule, Pipeline: w.Pipeline, Enabled: w.Enabled,
		ConsecutiveStrikes: w.ConsecutiveStrikes, LastRunAt: w.LastRunAt, LastStatus: w.LastStatus,
		ValueField: w.Compiled.ValueField, TagField: w.Compiled.TagField, HasAlert: w.Compiled.HasAlert,
		ValueType: w.Compiled.ValueType, AlertCondition: w.Compiled.AlertCondition,
		AlertThreshold: w.Compiled.AlertThreshold, ContentHash: w.ContentHash, Message: w.Message,
	}
	if w.Namespace != 0 {
		ns := w.Namespace
		out.NamespaceID = &ns
	}
	return out
}

// cronResultJSON is one recorded run on the wire.
type cronResultJSON struct {
	ID          uint
	CronWatchID uint
	RanAt       time.Time
	Stdout      string
	Stderr      string
	ExitCode    int
	DurationMs  int64
	Value       *float64
	Tag         string
	TextValue   *string
}

// cronAlert is the cutoff of the cron's alert stage, nil when it has none.
func cronAlert(w *cron.Watch) *chart.CronAlert {
	if w == nil || w.Compiled.AlertCondition == nil || w.Compiled.AlertThreshold == nil {
		return nil
	}
	return &chart.CronAlert{Condition: *w.Compiled.AlertCondition, Threshold: *w.Compiled.AlertThreshold}
}

func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if !s.requireGroup(w, session) {
		return
	}
	watches, err := s.readableCrons(r.Context(), session)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	limits := s.limitsOf(session)
	count, _ := s.crons.CountEnabled(session.Principal().Partition())
	// Each cron carries its path (<namespace>/<name>, or the bare name at the
	// org level), which is how every other call addresses it.
	nss, _ := s.sources.ListNamespaces(session.Principal().Partition())
	nsName := make(map[uint]string, len(nss))
	for _, n := range nss {
		nsName[n.ID] = n.Name
	}
	type cronItem struct {
		cronJSON
		Path string `json:"path"`
	}
	items := make([]cronItem, len(watches))
	for i, wt := range watches {
		items[i] = cronItem{cronJSON: cronOf(wt), Path: wt.Name}
		if wt.Namespace != 0 {
			items[i].Path = nsName[wt.Namespace] + "/" + wt.Name
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"crons":        items,
		"count":        count,
		"limit":        limits.MaxCrons,
		"min_interval": limits.MinCronInterval.String(),
	})
}

func (s *Server) handleGetWatch(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := cronPath(r)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	ok := s.requireGroup(w, session)
	if !ok {
		return
	}
	cw, err := s.cronAt(r.Context(), session, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(cronOf(*cw))
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
	if !s.requireGroup(w, session) {
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
	// non-blocking warning.Warning (the save already happened but there is
	// something to tell the user, e.g. the truncation lint); store.Split
	// separates that from a real rejection.
	content := "# schedule: " + req.Schedule + "\n" + req.Pipeline
	blocking, warnings := warning.Split(s.shellFS(r, session, nil).WriteFile(r.Context(), p, []byte(content)))
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
	bins  bin.Store
	owner shape.Added
}

func (r storeBinResolver) Source(name string) (string, string, bool) {
	fn, err := r.bins.Get(r.owner, name)
	if err != nil {
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
	if !s.requireGroup(w, session) {
		return
	}
	p, ok := mountPath("/cron.d", name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).Remove(r.Context(), p); err != nil {
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
	ok := s.requireGroup(w, session)
	if !ok {
		return
	}
	watch, err := s.cronAt(r.Context(), session, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	results, err := s.crons.ListResults(watch.ID, 50, "")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]cronResultJSON, len(results))
	for i, r := range results {
		out[i] = cronResultJSON{ID: r.ID, CronWatchID: r.WatchID, RanAt: r.RanAt, Stdout: r.Stdout, Stderr: r.Stderr,
			ExitCode: r.ExitCode, DurationMs: r.DurationMs, Value: r.Value, Tag: r.Tag, TextValue: r.TextValue}
	}
	json.NewEncoder(w).Encode(out)
}

// handleWatchLatest implements GET /api/crons/{name}/latest: the points of
// the cron's latest run, one per tag in table order, for the terminal
// dashboard's table and counter tiles.
func (s *Server) handleWatchLatest(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	ok := s.requireGroup(w, session)
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
	rows, err := s.crons.LatestRun(cronWatchID)
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
	if !s.requireGroup(w, session) {
		return
	}
	watch, err := s.cronAt(r.Context(), session, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if watch.Compiled.ValueType == cron.ValueText {
		http.Error(w, fmt.Sprintf("cron %q stores text (# type: text), so it has no numbers to chart; a dashboard table tile shows its latest values", name), http.StatusBadRequest)
		return
	}
	series, err := s.crons.Series(watch.ID, s.limitsOf(session).Clamp(start), end)
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
			Ts:    b.Start.UnixMilli(),
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
		"alert":       cronAlert(watch),
	})
}

// ── Namespace management ──────────────────────────────────────

func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if !s.requireGroup(w, session) {
		return
	}

	nss, err := s.sources.ListNamespaces(session.Principal().Partition())
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
	if !s.requireGroup(w, session) {
		return
	}

	if s.authorize(r.Context(), session, identity.ActionCreateNamespace, identity.Resource{}) != nil {
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

	if err := source.ValidateNamespaceName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ns, err := s.sources.FindOrCreateNamespace(session.Principal().Partition(), req.Name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if s.namespaceCreated != nil {
		s.namespaceCreated(r.Context(), session, ns.ID)
	}

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
	if s.authorize(r.Context(), session, identity.ActionDeleteNamespace, identity.Resource{}) != nil {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}
	if !s.requireGroup(w, session) {
		return
	}
	name := r.PathValue("name")
	ns, found, err := s.sources.GetNamespace(session.Principal().Partition(), name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	if r.URL.Query().Get("force") != "true" {
		owner := session.Principal().Partition()
		srcs, err := s.sources.ListSources(owner, source.Filter{Namespace: name})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		tokens, err := s.tokens.CountLiveTokens(owner, ns.ID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if len(srcs) > 0 || tokens > 0 {
			services := make([]string, len(srcs))
			for i, src := range srcs {
				services[i] = src.Service
			}
			slices.Sort(services)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error":   fmt.Sprintf("namespace %q is not empty", name),
				"sources": services,
				"tokens":  tokens,
			})
			return
		}
	}

	if err := s.sources.DeleteNamespace(r.Context(), session.Principal().Partition(), name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
