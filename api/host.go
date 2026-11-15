package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/config"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shellfs"
	"github.com/w3ft-net/weft/sources"
	"github.com/w3ft-net/weft/tenancy"
)

// This file is the surface a deployment's own routes are written against: the
// product's billing, admin and integrations live in the adapter (hosted:
// tenancy/hosted/hostedapi), not here, and reach the server through Routes and
// the accessors below. docs/plans/tenant-scope.md, Phase 4.

// Routes registers a deployment's own routes on the api's mux. The server hands
// one to each function passed to Server.Extend, after the core routes are in.
type Routes struct {
	s      *Server
	mux    *http.ServeMux // the whole api, before the access-log and CORS wrappers
	auth   *http.ServeMux // the JWT-authenticated routes behind the client-version gate
	gated  http.Handler   // auth behind AuthMiddleware and the client-version gate
	mounts map[string]bool
}

// Server is the server the routes belong to.
func (r *Routes) Server() *Server { return r.s }

// Config is the live runtime configuration (body caps, rate limits).
func (r *Routes) Config() *config.Runtime { return r.s.currentConfig() }

// Public registers h at pattern with no authentication.
func (r *Routes) Public(pattern string, h http.Handler) { r.mux.Handle(pattern, h) }

// Authed registers h at pattern behind AuthMiddleware and the client-version
// gate, like every core route a signed-in user calls. The path (up to the first
// wildcard) is routed to that gate on the outer mux.
func (r *Routes) Authed(pattern string, h http.Handler) {
	r.auth.Handle(pattern, h)
	_, path, ok := strings.Cut(pattern, " ")
	if !ok {
		path = pattern
	}
	if i := strings.Index(path, "{"); i >= 0 {
		path = path[:i]
	}
	if !r.mounts[path] {
		r.mounts[path] = true
		r.mux.Handle(path, r.gated)
	}
}

// Auth wraps h in the Identity's Authenticate without the client-version gate
// (the admin routes' stack).
func (r *Routes) Auth(h http.Handler) http.Handler {
	return r.s.ident().Authenticate(h)
}

// Limit applies a route's rate limit to h.
func (r *Routes) Limit(route RouteLimit, h http.Handler) http.Handler {
	return r.s.limiter.RateLimit(route, h)
}

// Extend registers fn to add routes when Handler builds the mux.
func (s *Server) Extend(fn func(*Routes)) { s.extend = append(s.extend, fn) }

// Producers is the registry of log producers. A deployment adds its own before
// the handler is built; the HTTPS ones mount at the routes they name.
func (s *Server) Producers() *sources.Registry { return s.producers }

// APIBaseURL is the api's own external URL.
func (s *Server) APIBaseURL() string { return s.apiBaseURL() }

// Authorize asks the Authorizer whether the session's user may take act.
func (s *Server) Authorize(ctx context.Context, session Session, act tenancy.Action, res tenancy.Resource) error {
	return s.authorize(ctx, session, act, res)
}

// CoordClient is the coordinator's gRPC client.
func (s *Server) CoordClient() pb.WeftCoordinatorClient { return s.coordClient }

// CoordCtx is the request's context carrying the caller's credentials to the
// coordinator.
func (s *Server) CoordCtx(r *http.Request) context.Context { return s.coordCtx(r) }

// ShellFS is the filesystem the shell writes through, bound to the session.
func (s *Server) ShellFS(r *http.Request, session Session, logServices []string) *shellfs.ShellFS {
	return s.shellFS(r, session, logServices)
}

// RequireConfigWrite refuses (403) unless the session may change /etc.
func (s *Server) RequireConfigWrite(w http.ResponseWriter, r *http.Request, session Session) bool {
	return s.requireConfigWrite(w, r, session)
}

// Readable is the names of the namespaces the session may read.
func (s *Server) Readable(ctx context.Context, session Session) ([]string, error) {
	return s.readable(ctx, session)
}

// LatestPoints is a cron's latest run as chart points.
func (s *Server) LatestPoints(cronID uint) ([]chart.LatestPoint, error) {
	return s.latestPoints(cronID)
}

// WriteFSError answers a failed write through the shell filesystem.
func WriteFSError(w http.ResponseWriter, err error) { writeFSError(w, err) }

// WriteJSON writes v as JSON.
func WriteJSON(w http.ResponseWriter, v any) error { return writeJSON(w, v) }

// BaseURL is the web app's external URL.
func (s *Server) BaseURL() string { return s.baseURL }

// ShellCommand is a command line the api answers itself instead of forwarding
// to the coordinator. It reports handled=false for a pipeline that is not its
// own; out is the text to show and err what to show on stderr.
type ShellCommand func(ctx context.Context, session Session, pipeline string) (handled bool, out string, err error)

// AddShellCommand registers a command set the shell exec route tries before it
// forwards a pipeline (hosted: the team commands adduser, deluser, usermod,
// groups and id).
func (s *Server) AddShellCommand(c ShellCommand) { s.shellCommands = append(s.shellCommands, c) }

// RecordAccessIdentity stamps who the request is for the access log. An
// Identity calls it once it has authenticated the request.
func RecordAccessIdentity(r *http.Request, label string) { recordAccessIdentity(r, label) }

// SetCoordClient installs the coordinator client in place of the one Connect
// dials: for an embedder with its own connection, and for tests with a fake.
func (s *Server) SetCoordClient(c pb.WeftCoordinatorClient) { s.coordClient = c }
