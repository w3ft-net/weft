package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// Managing the team and the plan is an owner's job. The shell's adduser and
// deluser already check; the REST endpoints behind the same actions did not.

func TestInvite_OnlyOwnersCanInvite(t *testing.T) {
	g := newGroupedOrg(t, store.TierYard)
	body := []byte(`{"email":"newhire@example.test"}`)

	for who, jwt := range map[string]string{"no group": g.plain, "bin": g.bin, "cron": g.cron, "etc": g.etc} {
		w := g.do(jwt, "POST", "/api/team/invite", body)
		if w.Code != http.StatusForbidden {
			t.Errorf("member with %s: status %d, want 403: %s", who, w.Code, w.Body.String())
		}
	}
	if m, _ := g.s.GetTeamMemberByEmail(g.org.ID, "newhire@example.test"); m != nil {
		t.Error("a refused invite still created a pending membership")
	}

	if w := g.do(g.owner, "POST", "/api/team/invite", body); w.Code != http.StatusCreated {
		t.Errorf("owner: status %d, want 201: %s", w.Code, w.Body.String())
	}
}

// The owner check lives in inviteMember itself, the function the REST
// endpoint and the shell's adduser both call, so no caller can skip it.
func TestInviteMember_RefusesNonOwners(t *testing.T) {
	g := newGroupedOrg(t, store.TierYard)

	_, err := g.srv.inviteMember(context.Background(), &auth.Session{
		UserID: "someone", Email: "someone@example.test",
		ActiveOrgID: g.org.ID, ActiveCustomerName: g.org.Name, IsOwner: false,
	}, "newhire@example.test")

	var ie *inviteErr
	if !errors.As(err, &ie) || ie.status != http.StatusForbidden {
		t.Errorf("err = %v, want a 403 inviteErr", err)
	}
}

func TestBilling_OnlyOwnersCanChangeThePlan(t *testing.T) {
	g := newGroupedOrg(t, store.TierYard)
	// A bogus tier makes the handler stop at validation for the owner without
	// reaching Stripe, so 400 here means "got past the owner check".
	body := []byte(`{"tier":"bogus"}`)

	for _, path := range []string{"/api/billing/checkout", "/api/billing/setup-intent"} {
		for who, jwt := range map[string]string{"no group": g.plain, "etc": g.etc} {
			if w := g.do(jwt, "POST", path, body); w.Code != http.StatusForbidden {
				t.Errorf("%s as member with %s: status %d, want 403: %s", path, who, w.Code, w.Body.String())
			}
		}
		if w := g.do(g.owner, "POST", path, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s as owner: status %d, want 400 from tier validation: %s", path, w.Code, w.Body.String())
		}
	}
}
