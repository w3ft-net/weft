package api

import (
	"bytes"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/auth"
)

// TestAccessLogAttributesUser pins the AccessLog identity fix.
//
// Before the fix AccessLog read auth.SessionFromContext off the parent
// request context, but AuthMiddleware injects the session into a child
// context that flows downward only — so every line logged user=anon,
// authenticated or not. That made the production access log useless for
// telling a real auth bypass apart from a logging artifact.
//
// After the fix an authenticated request must be attributed to the
// resolved user id; an anonymous request must still read user=anon.
func TestAccessLogAttributesUser(t *testing.T) {
	srv, s := testServer(t)
	h := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("acclog-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("acclog-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, fmt.Sprintf("acclog-%d", os.Getpid()))
	jwt, _ := auth.Generate(user, "test-secret")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Authenticated request → must attribute the user id.
	buf.Reset()
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	h.ServeHTTP(httptest.NewRecorder(), req)
	authedLine := buf.String()
	if !strings.Contains(authedLine, "user="+user.ID) {
		t.Errorf("authenticated request not attributed: want user=%s in access log, got:\n%s",
			user.ID, authedLine)
	}
	if strings.Contains(authedLine, "user=anon") {
		t.Errorf("authenticated request logged as anon:\n%s", authedLine)
	}

	// Unauthenticated request → must remain anon.
	buf.Reset()
	req = httptest.NewRequest("GET", "/api/auth/me", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if anonLine := buf.String(); !strings.Contains(anonLine, "user=anon") {
		t.Errorf("unauthenticated request should log user=anon, got:\n%s", anonLine)
	}
}
