package service

import (
	"io"
	"log"
	"net/http"
	"strings"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// Adapter is the wire-format-specific half of an HTTP ingest endpoint.
// The shared handler factory in makeIngestHandler runs auth, namespace
// scoping, env stamping, app onboarding, and ring-buffer hand-off; an
// adapter declares only how it pulls a token out of the request and
// how it decodes the body into LogRows.
//
// See docs/plans/http-ingest.md for the broader shape (JSON / Heroku /
// CloudWatch all share this interface).
type Adapter interface {
	// Auth pulls the bearer-equivalent secret out of the request.
	// Different adapters use different conventions — JSONAdapter reads
	// `Authorization: Bearer …`, HerokuAdapter reads the {token} URL
	// path component. Returns the raw token; the shared handler runs
	// store.ValidateAgentToken on it.
	Auth(req *http.Request) (string, error)

	// Parse decodes the body into one or more LogRows. Adapters do
	// NOT stamp customer/namespace/env — the shared handler does that
	// after the token resolves, so an adapter can't accidentally let a
	// client override these.
	Parse(body io.Reader, headers http.Header) ([]*pb.LogRow, error)

	// ContentLimit caps body size pre-decompression. Returned as
	// int64 so io.LimitReader takes it directly.
	ContentLimit() int64
}

// IngestHandler wires an Adapter into an HTTP handler that drops
// rows into the same drain pipeline IngestStream feeds. Per-row
// processing mirrors the gRPC path: customer/env stamped from the
// token, app prefix-checked against the token's namespace, SeenApps
// tracking + Provisioner.OnboardApp on first sight.
//
// Returned handler responds with:
//   - 204 No Content on success (Heroku's drain expects 2xx).
//   - 401 on auth failures.
//   - 400 on body parse failures (carries the underlying error message
//     so the operator can debug from the access log; not exposed
//     publicly via Heroku's drain UI but visible to weft operators).
//   - 413 when the body exceeds Adapter.ContentLimit().
func (s *Server) IngestHandler(a Adapter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, err := a.Auth(req)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		auth, err := s.Store.ValidateAgentToken(token)
		if err != nil {
			http.Error(w, "validate token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if auth == nil || auth.Customer == nil || auth.Customer.ID == 0 || auth.Customer.Name == "" {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}

		// Cap the body before any decompression so a malicious POST
		// can't hold a goroutine open for unbounded gunzip work.
		req.Body = http.MaxBytesReader(w, req.Body, a.ContentLimit())

		rows, err := a.Parse(req.Body, req.Header)
		if err != nil {
			// MaxBytesError surfaces as a *http.MaxBytesError when the
			// client over-runs the cap; map that to 413.
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

		s.stampAndIngest(rows, auth)
		w.WriteHeader(http.StatusNoContent)
	})
}

// stampAndIngest runs the per-row scoping the gRPC IngestStream path
// already runs: force the authoritative customer, stamp env if the
// token carries one, enforce the namespace prefix, and onboard new
// apps via SeenApps. Rejected rows (namespace violation) are logged
// and dropped — an HTTP intake can't fail the whole batch the way a
// streaming RPC can, since one POST is one transaction from the
// client's POV.
func (s *Server) stampAndIngest(rows []*pb.LogRow, auth *store.AgentAuthResult) {
	authCustomer := auth.Customer.Name
	authNamespace := auth.Namespace
	authEnv := auth.Env

	keep := rows[:0]
	for _, row := range rows {
		row.Customer = authCustomer
		if authEnv != "" {
			row.Env = authEnv
		}
		if row.App != authNamespace && !strings.HasPrefix(row.App, authNamespace+"/") {
			log.Printf("ingest-http: dropping row with out-of-scope app=%q (token namespace=%q)", row.App, authNamespace)
			continue
		}
		if s.Coord.SeenApps != nil && row.App != "" {
			if s.Coord.SeenApps.Add(row.Customer, row.App, row.Env) {
				log.Printf("ingest-http: new app discovered: customer=%s app=%s", row.Customer, row.App)
				if s.Coord.Provisioner != nil {
					if err := s.Coord.Provisioner.OnboardApp(row.App); err != nil {
						log.Printf("ingest-http: OnboardApp %q: %v", row.App, err)
					}
				}
			}
		}
		keep = append(keep, row)
	}
	if len(keep) > 0 {
		s.ingestBatch(keep)
	}
}
