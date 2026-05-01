package api

import (
	"fmt"
	"io"
	"net/http"

	"google.golang.org/grpc/metadata"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// Adapter is the wire-format-specific half of an HTTP ingest endpoint.
// IngestHandler runs the auth half (token lookup, namespace
// prefixing) and forwards parsed rows to the coordinator over the
// existing gRPC IngestStream — the same path weftd uses. An adapter
// declares only how it pulls a token out of the request and how it
// decodes the body into LogRows.
//
// See docs/plans/http-ingest.md for the broader shape (JSON / Heroku /
// CloudWatch all share this interface).
type Adapter interface {
	// Auth pulls the bearer-equivalent secret out of the request.
	// JSONAdapter reads `Authorization: Bearer …`; HerokuAdapter reads
	// the {token} URL path component.
	Auth(req *http.Request) (string, error)

	// Parse decodes the body into LogRows. Adapters set adapter-known
	// fields (App, Instance, Raw, TsUnixNs, etc.) but never set
	// Customer or Env — those come from the token and are stamped by
	// the coordinator's IngestStream before drain. Adapters MAY set
	// App in their own naming (e.g. HerokuAdapter emits `myapp/web`);
	// IngestHandler prefixes the token's namespace before forwarding,
	// so the coordinator sees `<namespace>/myapp/web` and the existing
	// scope check passes.
	Parse(body io.Reader, headers http.Header) ([]*pb.LogRow, error)

	// ContentLimit caps body size pre-decompression.
	ContentLimit() int64
}

// IngestHandler turns an Adapter into an HTTP handler. The flow:
//
//  1. Adapter.Auth → raw token from the request.
//  2. Validate the token against agent_tokens. Fast 401 on miss so
//     malformed drains don't proxy through to the coordinator.
//  3. Parse the body (capped by Adapter.ContentLimit()).
//  4. Prefix every row's App with the token's namespace, so adapter-
//     emitted names like `myapp/web` become `<namespace>/myapp/web`
//     and survive the coordinator's namespace-scope check.
//  5. Open a gRPC IngestStream to the coordinator with the same
//     bearer token in metadata; send rows; close.
//
// Response codes:
//   - 204 on success (Heroku's drain expects 2xx).
//   - 401 on auth failure.
//   - 400 on body-parse failure.
//   - 413 when the body exceeds ContentLimit.
//   - 502 on coordinator-stream failure (transient; client retries).
func (s *Server) IngestHandler(a Adapter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, err := a.Auth(req)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		auth, err := s.store.ValidateAgentToken(token)
		if err != nil {
			http.Error(w, "validate token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if auth == nil || auth.Customer == nil || auth.Customer.ID == 0 || auth.Customer.Name == "" {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}
		namespace := auth.Namespace
		if namespace == "" {
			http.Error(w, "token has no namespace bound", http.StatusUnauthorized)
			return
		}

		// Cap body before decompression so a malicious POST can't hold
		// a goroutine open for unbounded gunzip work.
		req.Body = http.MaxBytesReader(w, req.Body, a.ContentLimit())

		rows, err := a.Parse(req.Body, req.Header)
		if err != nil {
			if _, over := err.(*http.MaxBytesError); over {
				http.Error(w, "body exceeds content limit", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(rows) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Prefix the namespace so coordinator-side scope check passes.
		// Adapter emits `myapp/web`; coordinator expects rows to live
		// under the token's namespace, so we ship `<ns>/myapp/web`.
		for _, r := range rows {
			r.App = prefixNamespace(namespace, r.App)
		}

		// Forward to coordinator via the existing gRPC IngestStream.
		// Same bearer token in metadata so authenticateStream's path
		// is exactly the path weftd takes — customer/env stamping and
		// SeenApps onboarding happen there, not here.
		ctx := metadata.AppendToOutgoingContext(req.Context(), "authorization", "Bearer "+token)
		stream, err := s.coordClient.IngestStream(ctx)
		if err != nil {
			http.Error(w, "open ingest stream: "+err.Error(), http.StatusBadGateway)
			return
		}
		for _, r := range rows {
			if err := stream.Send(r); err != nil {
				http.Error(w, "send row: "+err.Error(), http.StatusBadGateway)
				return
			}
		}
		if _, err := stream.CloseAndRecv(); err != nil {
			http.Error(w, "close stream: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}

// prefixNamespace ensures `app` lives under `ns`. Mirrors the helper
// weftd uses on the inbound side. An app already prefixed (or already
// equal to ns) is left alone.
func prefixNamespace(ns, app string) string {
	if ns == "" || app == "" {
		return app
	}
	if app == ns {
		return app
	}
	if len(app) > len(ns)+1 && app[:len(ns)] == ns && app[len(ns)] == '/' {
		return app
	}
	return fmt.Sprintf("%s/%s", ns, app)
}
