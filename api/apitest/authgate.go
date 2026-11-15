package apitest

import (
	"net/http"
	"testing"
)

// Auth runs the authentication contract of the routes the api registers.
func Auth(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"EndpointsRequireAuth", endpointsRequireAuth},
	})
}

// TestEndpointsRequireAuth enumerates every HTTP route the API server
// registers and asserts the authentication contract for each one when
// called with NO Authorization header.
//
// The contract:
//
//   - private == true   → an anonymous caller MUST be rejected by the
//     auth gate. AuthMiddleware returns 401 ("missing authorization")
//     before the handler ever runs, so the only acceptable status is
//     401. Anything else (200 with tenant data, or even a 404 because
//     the route was registered on the inner mux but never mounted
//     behind AuthMiddleware) is a security defect.
//
//   - private == false  → a public route. It may return anything EXCEPT
//     a 401 caused by the missing Authorization header (it can 400 on a
//     bad body, 200, etc). We only assert it is not gated.
//
// This is the failing-test-first reproduction for the production report:
// `GET /api/sources/pending` returning 200 to an unauthenticated caller
// while `GET /api/auth/me` correctly returned 401. The access log's
// `user=anon` is a logging artifact (AccessLog reads the session off the
// parent context, which never receives it), so it cannot prove or
// disprove a bypass — this test can.
func endpointsRequireAuth(t *testing.T, s Subject) {

	// gate classifies the auth contract a route is supposed to honor.
	type gate int
	const (
		public    gate = iota // reachable with no credentials at all
		session               // behind AuthMiddleware: anon MUST get 401
		pathToken             // self-authenticates via a token in the URL
		// path; a bogus token MUST be rejected with 401, so it is
		// neither "public" nor session-gated.
	)

	type route struct {
		method string
		path   string
		gate   gate
	}

	// Path params are concretized with throwaway values; AuthMiddleware
	// rejects before the handler parses them, so the values are inert
	// for the private cases.
	// Path params are concretized with throwaway values; AuthMiddleware
	// rejects before the handler parses them, so the values are inert
	// for the session-gated cases.
	routes := []route{
		// ── Public: reachable with no credentials ────────────────
		{"GET", "/health", public},

		// ── Path-token authed: bogus token MUST be rejected ──────
		{"POST", "/api/ingest/heroku/sometoken", pathToken},

		// ── Session-gated: anonymous caller MUST get 401 ─────────
		{"GET", "/api/fs/list", session},
		{"GET", "/api/fs/read", session},
		{"PUT", "/api/fs/write", session},
		{"DELETE", "/api/fs/remove", session},
		{"GET", "/api/tokens", session},
		{"POST", "/api/tokens", session},
		{"DELETE", "/api/tokens/1", session},
		{"GET", "/api/apps", session},
		{"GET", "/api/ingestion-status", session},
		{"POST", "/api/shell/exec", session},
		{"GET", "/api/functions", session},
		{"GET", "/api/functions/myfunc", session},
		{"PUT", "/api/functions/myfunc", session},
		{"DELETE", "/api/functions/myfunc", session},
		{"GET", "/api/crons", session},
		{"GET", "/api/crons/mywatch", session},
		{"PUT", "/api/crons/mywatch", session},
		{"DELETE", "/api/crons/mywatch", session},
		{"GET", "/api/crons/mywatch/results", session},
		{"GET", "/api/crons/mywatch/chart", session},
		{"GET", "/api/notify", session},
		{"PUT", "/api/notify", session},
		{"PUT", "/api/integrations/webhook-secrets", session},
		{"DELETE", "/api/integrations/webhook-secrets/token", session},
		{"GET", "/api/namespaces", session},
		{"POST", "/api/namespaces", session},
		{"DELETE", "/api/namespaces/ns", session},
		{"GET", "/api/sources/pending", session},
		{"GET", "/api/sources/pending/count", session},
		{"POST", "/api/sources/ns/app/configure", session},
		{"GET", "/api/sources", session},
		{"POST", "/api/sources/cloudwatch/sample", session},
		{"POST", "/api/sources/cloudwatch", session},
		{"DELETE", "/api/sources/ns/app", session},
	}

	for _, rt := range routes {
		name := rt.method + " " + rt.path
		t.Run(name, func(t *testing.T) {
			// Deliberately NO Authorization header.
			got := s.Anon(rt.method, rt.path, []byte("")).Code

			switch rt.gate {
			case session:
				if got != http.StatusUnauthorized {
					t.Errorf("SESSION route %s served an unauthenticated caller status %d; want 401. "+
						"A non-401 here means the route is not behind AuthMiddleware "+
						"(200 = data leak; 404 = handler registered on the inner mux but "+
						"never mounted behind AuthMiddleware, so it is unreachable rather than gated).",
						name, got)
				}
			case pathToken:
				if got != http.StatusUnauthorized {
					t.Errorf("PATH-TOKEN route %s accepted a bogus token with status %d; want 401.",
						name, got)
				}
			case public:
				if got == http.StatusUnauthorized {
					t.Errorf("PUBLIC route %s rejected an anonymous caller with 401; "+
						"it must remain reachable without auth.", name)
				}
			}
		})
	}
}
