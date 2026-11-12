//go:build !defaultshape

package api

import (
	"net/http"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// Webhook secrets are the credentials a webhook alert target delivers with, so
// changing them takes the group that owns notify.yaml: configmgr.
func TestWebhookSecrets_RequireConfigmgr(t *testing.T) {
	routes := []struct {
		name, method, path, body string
		seed                     func(*groupedOrg)
	}{
		{name: "save", method: "PUT", path: "/api/integrations/webhook-secrets", body: `{"name":"token","value":"s3cret"}`},
		{
			name: "delete", method: "DELETE", path: "/api/integrations/webhook-secrets/token",
			seed: func(g *groupedOrg) { g.s.SaveWebhookSecret(g.org.ID, "token", "s3cret") },
		},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			g := newGroupedOrg(t, store.TierYard)
			if tc.seed != nil {
				tc.seed(g)
			}
			for who, jwt := range map[string]string{"no group": g.plain, "cron": g.cron} {
				if w := g.do(jwt, tc.method, tc.path, []byte(tc.body)); w.Code != http.StatusForbidden {
					t.Errorf("member with %s: status %d, want 403: %s", who, w.Code, w.Body.String())
				}
			}
			if w := g.do(g.etc, tc.method, tc.path, []byte(tc.body)); w.Code >= 300 {
				t.Errorf("member with configmgr: status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// A denied save must not have written anything.
func TestWebhookSecrets_DeniedSaveWritesNothing(t *testing.T) {
	g := newGroupedOrg(t, store.TierYard)
	g.do(g.plain, "PUT", "/api/integrations/webhook-secrets", []byte(`{"name":"token","value":"s3cret"}`))
	if names, _ := g.s.ListWebhookSecretNames(g.org.ID); len(names) != 0 {
		t.Errorf("denied webhook secret save wrote %v", names)
	}
}
