//go:build !defaultshape

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// ingestControl posts a Firehose CONTROL_MESSAGE, which authenticates and
// then returns without forwarding anything — the auth cost in isolation.
func ingestControl(t *testing.T, srv *Server, token string) int {
	t.Helper()
	body := firehoseBody("req-control", plainEnvelope(t, map[string]any{"messageType": "CONTROL_MESSAGE"}))
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)
	return w.Code
}

// TestIngestHandler_WarmTokenTouchesNoAuthTables: every HTTP ingest request
// validates its token, and the coordinator validates it again on the
// forwarded stream. With the auth cache on, a warm token must cost the API
// no agent_tokens lookups at all.
func TestIngestHandler_WarmTokenTouchesNoAuthTables(t *testing.T) {
	srv, s := testServer(t)
	_, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient
	s.EnableAuthCache()

	token, _ := setupIngestOrg(t, s)

	var stmts atomic.Int64
	remove := s.OnSQLForTest("count_ingest_auth", func(sql string) {
		if strings.Contains(sql, `"agent_tokens"`) {
			stmts.Add(1)
		}
	})
	defer remove()

	if code := ingestControl(t, srv, token); code != http.StatusOK {
		t.Fatalf("cold request status = %d", code)
	}
	if stmts.Load() == 0 {
		t.Fatal("cold request issued no agent_tokens statements; counter is not wired")
	}
	warm := stmts.Load()
	for i := 0; i < 20; i++ {
		if code := ingestControl(t, srv, token); code != http.StatusOK {
			t.Fatalf("warm request %d status = %d", i, code)
		}
	}
	if extra := stmts.Load() - warm; extra != 0 {
		t.Errorf("20 warm requests issued %d agent_tokens statements, want 0", extra)
	}
}

// TestIngestHandler_RevokeTakesEffectImmediatelyInAPI: the revoke handler
// runs in this process, so it must invalidate this process's cache rather
// than leaving the token valid until the TTL expires.
func TestIngestHandler_RevokeTakesEffectImmediatelyInAPI(t *testing.T) {
	srv, s := testServer(t)
	_, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient
	s.EnableAuthCache()

	cust, err := s.FindOrCreateOrg("revoke-immediate")
	if err != nil {
		t.Fatal(err)
	}
	ns, _ := s.FindOrCreateNamespace(cust.ID, "test")
	token, tok, err := s.CreateAgentToken(cust.ID, ns.ID, "revoke-me", "")
	if err != nil {
		t.Fatal(err)
	}

	if code := ingestControl(t, srv, token); code != http.StatusOK {
		t.Fatalf("pre-revoke status = %d", code)
	}

	user, err := s.CreateVerifiedUser("revoke-immediate@example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/tokens/"+tok.PublicID, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", w.Code)
	}

	if code := ingestControl(t, srv, token); code != http.StatusUnauthorized {
		t.Errorf("post-revoke status = %d, want 401 (cache served a revoked token)", code)
	}
}
