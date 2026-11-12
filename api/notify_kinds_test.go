//go:build !defaultshape

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

var notifyKindsSeq atomic.Int64

// putNotify saves aliases through PUT /api/notify as a new org's owner and
// returns the status and body.
func putNotify(t *testing.T, srv *Server, s *store.Store, yaml string) (int, string) {
	t.Helper()
	n := notifyKindsSeq.Add(1)
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notifykinds-%d-%d", os.Getpid(), n))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("notifykinds-%d-%d@example.com", os.Getpid(), n))
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	jwt, _ := auth.Generate(user, "test-secret")
	body, _ := json.Marshal(map[string]string{"aliases_yaml": yaml})
	req := httptest.NewRequest(http.MethodPut, "/api/notify", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

const notifyKindsSystem = "system:\n  email: [oncall@example.test]\n"

// An api with no deployment kinds is the open build: it sends webhook and
// email, so a save naming slack is refused rather than accepted and skipped
// when an alert fires.
func TestNotifyConfig_OpenBuildRefusesAKindItDoesNotSend(t *testing.T) {
	srv, s := testServer(t)
	srv.SetAlertKinds(nil)
	code, body := putNotify(t, srv, s, notifyKindsSystem+"ops:\n  slack: ['#ops']\n")
	if code != http.StatusBadRequest || !strings.Contains(body, `"slack"`) {
		t.Fatalf("PUT slack = %d %q, want 400 naming slack", code, body)
	}
}

// The hosted kinds are accepted, and their entries are still checked.
func TestNotifyConfig_HostedKindsAreAcceptedAndChecked(t *testing.T) {
	srv, s := testServer(t) // testServer installs the kinds hostedapi does
	if code, body := putNotify(t, srv, s, notifyKindsSystem+"ops:\n  slack: ['#ops']\n  pagerduty: [primary]\n"); code != http.StatusOK {
		t.Fatalf("PUT hosted kinds = %d %q, want 200", code, body)
	}
	code, body := putNotify(t, srv, s, notifyKindsSystem+"ops:\n  pagerduty:\n    - {name: primary, severity: loud}\n")
	if code != http.StatusBadRequest || !strings.Contains(body, "severity") {
		t.Fatalf("PUT bad severity = %d %q, want 400 naming severity", code, body)
	}
}
