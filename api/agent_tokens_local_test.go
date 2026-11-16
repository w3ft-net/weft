package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// The agent routes authenticate against any token.Store: here the open
// build's SQLite one, with no Postgres.
func TestAgentTokenMiddleware_OverTheLocalTokenStore(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sources, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := defaults.NewTokenBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := sources.FindOrCreateNamespace(shape.Added{}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := tokens.CreateToken(shape.Added{}, ns.ID, "agent", "")
	if err != nil {
		t.Fatal(err)
	}

	var seen *AgentAuthResult
	h := AgentTokenMiddleware(tokens, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = GetAgentAuth(r)
	}))
	call := func(auth string) int {
		seen = nil
		req := httptest.NewRequest(http.MethodGet, "/api/agent/x", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("Bearer " + raw); code != http.StatusOK || seen == nil || seen.Namespace != "ops" {
		t.Fatalf("a valid token: status %d, auth %+v", code, seen)
	}
	for name, auth := range map[string]string{"none": "", "unknown": "Bearer wft_00000000000000000000000000000000", "wrong scheme": "Token " + raw} {
		if code := call(auth); code != http.StatusUnauthorized || seen != nil {
			t.Errorf("%s: status %d, auth %+v, want 401", name, code, seen)
		}
	}
	if err := tokens.RevokeToken(shape.Added{}, tok.PublicID); err != nil {
		t.Fatal(err)
	}
	if code := call("Bearer " + raw); code != http.StatusUnauthorized {
		t.Errorf("a revoked token: status %d, want 401", code)
	}
}
