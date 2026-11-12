package hostedapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The hosted routes keep the authentication contract they had in the core api
// (api.TestEndpointsRequireAuth): a session route answers an anonymous caller
// 401, and a public one never does. A 404 on a session route means it was
// registered on the inner mux but never mounted behind AuthMiddleware.
func TestHostedRoutesRequireAuth(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Handler()

	for _, rt := range []struct {
		method, path string
		public       bool
	}{
		{"POST", "/api/auth/login", true},
		{"POST", "/api/auth/signup", true},
		{"POST", "/api/auth/verify", true},
		{"POST", "/api/auth/device", true},
		{"POST", "/api/auth/device/poll", true},
		{"POST", "/api/team/accept", true}, // invite token in body
		{"GET", "/api/auth/me", false},
		{"POST", "/api/auth/device/approve", false},
		{"POST", "/api/auth/resend-verification", false},
		{"POST", "/api/account/welcome-dismiss", false},
		{"POST", "/api/onboarding/advance", false},
		{"POST", "/api/onboarding/dismiss", false},
		{"POST", "/api/onboarding/graduation-seen", false},
		{"POST", "/api/shell/builtin", false},
		{"POST", "/api/billing/webhook", true},            // Stripe: own signature auth
		{"GET", "/api/integrations/slack/callback", true}, // OAuth state token
		{"POST", "/api/billing/checkout", false},
		{"POST", "/api/billing/setup-intent", false},
		{"GET", "/api/billing/usage", false},
		{"GET", "/api/integrations", false},
		{"POST", "/api/integrations/slack/connect", false},
		{"DELETE", "/api/integrations/slack", false},
		{"PUT", "/api/integrations/pagerduty", false},
		{"DELETE", "/api/integrations/pagerduty", false},
		{"DELETE", "/api/integrations/pagerduty/myrule", false},
		{"GET", "/api/admin/customers", false},
		{"GET", "/api/notices", false},
		{"POST", "/api/notices/1/seen", false},
		{"PUT", "/api/namespaces/ns/permissions/user-1", false},
		{"GET", "/api/permissions", false},
		{"PUT", "/api/permissions", false},
		{"GET", "/api/team", false},
		{"PUT", "/api/team/name", false},
		{"POST", "/api/team/invite", false},
		{"DELETE", "/api/team/some-id", false},
		{"GET", "/api/shared-links/some-slug", false},
		{"GET", "/api/admin/buckets", false},
	} {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("")))
			got := w.Result().StatusCode
			if rt.public && got == http.StatusUnauthorized {
				t.Errorf("public route answered an anonymous caller 401")
			}
			if !rt.public && got != http.StatusUnauthorized {
				t.Errorf("session route answered an anonymous caller %d, want 401", got)
			}
		})
	}
}
