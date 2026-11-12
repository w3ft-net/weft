package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/internal/clientwire"
	"github.com/graybeardcorp/weft/store"
)

type contextKey string

// writeJSON is a tiny helper to keep the middleware body readable.
func writeJSON(w http.ResponseWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// agentAuthKey holds the validated agent auth result in the request context.
const agentAuthKey contextKey = "agentAuth"

// AgentAuthResult is the identity derived from a validated agent token.
type AgentAuthResult struct {
	OrgID     store.OrgID
	OrgName   string
	Namespace string
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
			OrgID:     storeResult.Organization.ID,
			OrgName:   storeResult.Organization.Name,
			Namespace: storeResult.Namespace,
		}
		recordAccessIdentity(r, "agent:"+result.OrgName+"/"+result.Namespace)
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
// context before dispatching. Downstream middleware (the Identity,
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
		// trace_id rides on r.Context() once TraceMiddleware (which wraps
		// this handler) has established one, and is omitted otherwise.
		weftsdk.Infof(r.Context(), "access %s %s %d %s user=%s", r.Method, r.URL.Path, rw.status, time.Since(start), user)
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
