package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/internal/clientwire"
	"github.com/graybeardcorp/weft/store"
)

type contextKey string

// ActiveOrgHeader is the request header that selects which org
// (customer) the user is acting as for this call. CLI sends it from
// the --org flag; the web UI from a navbar selector. When absent, the
// middleware falls back to the user's last_active_org_id, then to
// their oldest membership.
const ActiveOrgHeader = "X-Active-Org"

// AuthMiddleware validates JWT tokens from the Authorization header,
// resolves the caller's full identity (including which org to scope
// to) from the DB, and stashes both the (slim) claims and the
// resolved Session on the request context. Handlers retrieve via
// GetSession (REST) or auth.SessionFromContext (any protocol).
//
// When the resolved active org differs from the user's stored
// last_active_org_id, the middleware writes the new value back
// — that way reconnecting later (without an explicit X-Active-Org
// header) lands the user in the org they last operated in.
func AuthMiddleware(st *store.Store, secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(hdr, "Bearer ")
		claims, err := auth.Validate(token, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		orgSelector := r.Header.Get(ActiveOrgHeader)
		session, err := auth.BuildSession(st, claims.UserID, orgSelector)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrOrgNotMember):
				http.Error(w, "not a member of the requested org", http.StatusForbidden)
			case errors.Is(err, auth.ErrNoMembership):
				http.Error(w, "no org memberships", http.StatusForbidden)
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}
		persistLastActive(st, session)
		recordAccessIdentity(r, session.UserID)
		ctx := auth.WithClaims(r.Context(), claims)
		ctx = auth.WithSession(ctx, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// persistLastActive flips users.last_active_org_id when the resolved
// active org doesn't match what the session loaded. It is synchronous,
// but the write only happens on a change, so the common request pays
// nothing; failures log but never block the request (the next request
// will retry).
func persistLastActive(st *store.Store, session *auth.Session) {
	// An org-less session (a superadmin with no membership) has no active
	// customer to pin. Writing 0 trips the last_active_org_id foreign
	// key, so there's nothing to persist.
	if session.ActiveOrgID == 0 {
		return
	}
	if session.LastActiveOrgID != nil && *session.LastActiveOrgID == session.ActiveOrgID {
		return
	}
	if err := st.UpdateUserLastActiveOrg(session.UserID, session.ActiveOrgID); err != nil {
		log.Printf("update last_active_org_id for %s: %v", session.UserID, err)
	}
}

// RequireEmailVerified gates state-changing endpoints on a verified
// email. Reads the Session that AuthMiddleware inserts (which already
// reflects the current DB state), so a user who verified after their
// JWT was issued passes this gate without re-logging in.
func RequireEmailVerified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := auth.SessionFromContext(r.Context())
		if session == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !session.EmailVerified {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = writeJSON(w, map[string]any{
				"code":    "email_verification_required",
				"message": "Verify your email to unlock this feature.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSuperadmin gates admin endpoints on is_superadmin. Reads the
// Session that AuthMiddleware already materialized from the DB.
func RequireSuperadmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := auth.SessionFromContext(r.Context())
		if session == nil || !session.IsSuperadmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON is a tiny helper to keep the middleware body readable.
func writeJSON(w http.ResponseWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// GetClaims extracts JWT claims from the REST request context. Most
// handlers should use GetSession instead — the slim claims only carry
// the user ID, while Session has the full resolved identity.
func GetClaims(r *http.Request) *auth.Claims {
	return auth.FromContext(r.Context())
}

// GetSession extracts the resolved Session from the REST request
// context. Returns nil if AuthMiddleware was not applied to the route.
func GetSession(r *http.Request) *auth.Session {
	return auth.SessionFromContext(r.Context())
}

// agentAuthKey holds the validated agent auth result in the request context.
const agentAuthKey contextKey = "agentAuth"

// AgentAuthResult is the identity derived from a validated agent token.
type AgentAuthResult struct {
	OrgID        store.OrgID
	CustomerName string
	Namespace    string
}

// AgentTokenMiddleware validates weftd agent tokens (wft_...) from the
// Authorization header, resolving (customer, namespace) from the token.
func AgentTokenMiddleware(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(hdr, "Bearer ")
		storeResult, err := s.ValidateAgentToken(token)
		if err != nil || storeResult == nil {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}
		result := &AgentAuthResult{
			OrgID:        storeResult.Organization.ID,
			CustomerName: storeResult.Organization.Name,
			Namespace:    storeResult.Namespace,
		}
		recordAccessIdentity(r, "agent:"+result.CustomerName+"/"+result.Namespace)
		ctx := context.WithValue(r.Context(), agentAuthKey, result)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetAgentAuth extracts agent auth from the request context.
func GetAgentAuth(r *http.Request) *AgentAuthResult {
	result, _ := r.Context().Value(agentAuthKey).(*AgentAuthResult)
	return result
}

// accessIdentity is a mutable holder AccessLog plants on the request
// context before dispatching. Downstream middleware (AuthMiddleware,
// AgentTokenMiddleware) resolve identity into a child context that
// never propagates back up — so AccessLog can't read the session
// directly. Writing through this pointer does cross that boundary:
// the value lives in the parent context AccessLog still holds, and the
// child contexts only wrap (never copy) it.
type accessIdentity struct{ label string }

type accessIdentityKeyType struct{}

var accessIdentityKey = accessIdentityKeyType{}

// recordAccessIdentity stamps the caller's identity for the access log.
// No-op if AccessLog isn't in the chain (e.g. gRPC), so callers don't
// need to branch.
func recordAccessIdentity(r *http.Request, label string) {
	if id, ok := r.Context().Value(accessIdentityKey).(*accessIdentity); ok {
		id.label = label
	}
}

// AccessLog logs one line per HTTP request: method, path, status, duration,
// user. Wraps the entire mux (outside CORS) so auth failures, 404s, and
// OPTIONS preflights are all visible. The `log` package supplies the
// timestamp prefix.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// "api.access" bookends this request with weftsdk's own "span
		// start"/"span done" lines when traced (no-op otherwise) — the
		// gap-histogram display needs both ends of every hop to
		// attribute time, not just this one's total duration. See
		// coordinator/compute/shard for the matching span at each of
		// those layers.
		ctx, span := weftsdk.StartSpan(r.Context(), "api.access")
		defer span.End()
		r = r.WithContext(ctx)
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		id := &accessIdentity{}
		r = r.WithContext(context.WithValue(r.Context(), accessIdentityKey, id))
		next.ServeHTTP(rw, r)
		if rw.status >= 500 {
			span.RecordError(fmt.Errorf("status %d", rw.status))
		}
		user := id.label
		if user == "" {
			user = "anon"
		}
		// trace_id is only present once TraceMiddleware (which wraps this
		// handler) has established one — omit the field entirely rather
		// than print "trace_id=" for every untraced request.
		if traceID, ok := weftsdk.TraceIDFromContext(r.Context()); ok {
			log.Printf("access %s %s %d %s user=%s trace_id=%s", r.Method, r.URL.Path, rw.status, time.Since(start), user, traceID)
			return
		}
		log.Printf("access %s %s %d %s user=%s", r.Method, r.URL.Path, rw.status, time.Since(start), user)
	})
}

// statusRecorder captures the response status so AccessLog can log it.
// Defaults to 200 because handlers that write a body without an explicit
// WriteHeader implicitly send 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the wrapped writer so http.ResponseController (and any
// outer wrapper following the same convention) can reach the real
// server writer through this layer.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Flush keeps the SSE shell handler's `w.(http.Flusher)` assertion
// satisfied AND actually delivers the flush. A one-level
// `s.ResponseWriter.(http.Flusher)` assertion silently no-ops whenever
// the wrapped writer exposes flushing only via Unwrap() — every
// non-trivial wrapper and the HTTP/2 server writer — which stalls every
// streaming endpoint while leaving w.(http.Flusher) satisfied (so no
// 500, just a hung stream). http.NewResponseController walks the
// Unwrap() chain the way the stdlib intends.
func (s *statusRecorder) Flush() {
	//nolint:errcheck // ErrNotSupported only when nothing in the chain
	// flushes; nothing actionable here and matches http.Flusher's
	// error-free signature.
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

// allowedRequestHeaders is the Access-Control-Allow-Headers value,
// derived from the shared client wire contract so it cannot drift from
// what the shell client actually sends. See package clientwire.
var allowedRequestHeaders = strings.Join(clientwire.AuthHeaders, ", ")

// CORSMiddleware adds CORS headers for the SPA. Allows the configured
// origin plus localhost for development.
func CORSMiddleware(origin string, next http.Handler) http.Handler {
	allowed := map[string]bool{
		origin: true,
	}
	// Allow common dev origins.
	for _, port := range []string{"5173", "5174", "3000"} {
		allowed["http://localhost:"+port] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqOrigin := r.Header.Get("Origin")
		if allowed[reqOrigin] {
			w.Header().Set("Access-Control-Allow-Origin", reqOrigin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", allowedRequestHeaders)
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
