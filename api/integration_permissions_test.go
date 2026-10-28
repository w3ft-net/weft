package api

import (
	"net/http"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// Integrations (Slack, PagerDuty, webhook secrets) are the credentials the
// /etc/notify.yaml aliases point at, so changing them takes the same group as
// changing notify.yaml: configmgr. They used to take any member.
func TestIntegrationWrites_RequireConfigmgr(t *testing.T) {
	routes := []struct {
		name, method, path, body string
		seed                     func(*groupedOrg)
	}{
		{name: "pagerduty save", method: "PUT", path: "/api/integrations/pagerduty", body: `{"name":"ops","key":"abc123"}`},
		{name: "pagerduty delete", method: "DELETE", path: "/api/integrations/pagerduty/ops"},
		{name: "pagerduty disconnect", method: "DELETE", path: "/api/integrations/pagerduty"},
		{name: "slack connect", method: "POST", path: "/api/integrations/slack/connect"},
		{name: "slack disconnect", method: "DELETE", path: "/api/integrations/slack"},
		{name: "webhook secret save", method: "PUT", path: "/api/integrations/webhook-secrets", body: `{"name":"token","value":"s3cret"}`},
		{
			name: "webhook secret delete", method: "DELETE", path: "/api/integrations/webhook-secrets/token",
			seed: func(g *groupedOrg) { g.s.SaveWebhookSecret(g.org.ID, "token", "s3cret") },
		},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			g := newGroupedOrg(t, store.TierYard)
			g.srv.SlackClientID = "slack-client" // connect answers 503 when unconfigured
			if tc.seed != nil {
				tc.seed(g)
			}

			for who, jwt := range map[string]string{"no group": g.plain, "bin": g.bin, "cron": g.cron} {
				w := g.do(jwt, tc.method, tc.path, []byte(tc.body))
				if w.Code != http.StatusForbidden {
					t.Errorf("member with %s: status %d, want 403: %s", who, w.Code, w.Body.String())
				}
			}

			w := g.do(g.etc, tc.method, tc.path, []byte(tc.body))
			if w.Code >= 300 {
				t.Errorf("member with configmgr: status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// A denied save must not have written anything.
func TestIntegrationWrites_DeniedSaveWritesNothing(t *testing.T) {
	g := newGroupedOrg(t, store.TierYard)

	g.do(g.plain, "PUT", "/api/integrations/webhook-secrets", []byte(`{"name":"token","value":"s3cret"}`))
	g.do(g.plain, "PUT", "/api/integrations/pagerduty", []byte(`{"name":"ops","key":"abc123"}`))

	if names, _ := g.s.ListWebhookSecretNames(g.org.ID); len(names) != 0 {
		t.Errorf("denied webhook secret save wrote %v", names)
	}
	var n int64
	g.s.DB().WithContext(store.WithOrg(t.Context(), g.org.ID)).Model(&store.PagerDutyIntegration{}).Count(&n)
	if n != 0 {
		t.Errorf("denied pagerduty save wrote %d rows", n)
	}
}
