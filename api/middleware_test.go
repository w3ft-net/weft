package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORS_AllowsActiveOrgHeader pins the regression where the welcome
// modal's Start-the-tour click fired a cross-origin /api/auth/me with
// the X-Active-Org header set to "loom" (from switchOrg's localStorage
// write + reload). The browser's preflight to api.w3ft.net came back
// with Access-Control-Allow-Headers: Authorization, Content-Type —
// X-Active-Org was missing, so the actual request never went out, the
// frontend's getMe rejected, and AuthContext cleared the token and
// bounced the user to /login.
//
// Every cross-origin authed call uses this header (api.ts:38 sets it
// from localStorage on every request), so the gap affected more than
// the tour — anyone who'd ever switched orgs would hit it on the next
// page reload. The fix is just to include the header in the preflight
// response.
func TestCORS_AllowsActiveOrgHeader(t *testing.T) {
	handler := CORSMiddleware("https://w3ft.net", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/auth/me", nil)
	req.Header.Set("Origin", "https://w3ft.net")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "x-active-org")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight: expected 204, got %d", w.Code)
	}
	allow := w.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(strings.ToLower(allow), "x-active-org") {
		t.Fatalf("preflight Access-Control-Allow-Headers must include X-Active-Org (got %q); without it the browser blocks every cross-origin authed call once the SPA has an org pinned",
			allow)
	}
}
