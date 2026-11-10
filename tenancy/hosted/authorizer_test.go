package hosted

import (
	"context"
	"errors"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

func TestAuthorize(t *testing.T) {
	db := newStore(t)
	org, _ := db.FindOrCreateOrg("hosted-authz")
	ns, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	db.FindOrCreateNamespace(org.ID, "beta")
	mk := func(email string, role orgs.Role) string {
		u, err := db.CreateVerifiedUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddTeamMemberWithEmail(org.ID, u.ID, u.Email, role); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	owner := mk("o@authz.test", store.RoleOwner)
	plain := mk("p@authz.test", orgs.RoleMember)
	maker := mk("m@authz.test", orgs.RoleMember)
	tokener := mk("t@authz.test", orgs.RoleMember)
	if err := db.DB().Exec(`INSERT INTO org_permissions (org_id, user_id, can_create_namespaces) VALUES (?, ?, true)`, org.ID, maker).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNamespacePermission(&store.NamespacePermission{UserID: tokener, NamespaceID: ns.ID, CanRead: true, CanCreateTokens: true}); err != nil {
		t.Fatal(err)
	}
	super, _ := db.CreateVerifiedUser("s@authz.test")
	if err := db.DB().Exec(`UPDATE users SET is_superadmin = true WHERE id = ?`, super.ID).Error; err != nil {
		t.Fatal(err)
	}

	z := Authorizer{Store: db}
	cases := []struct {
		who  string
		user string
		act  tenancy.Action
		res  tenancy.Resource
		want bool
	}{
		{"owner manages team", owner, ActionManageTeam, tenancy.Resource{}, true},
		{"member cannot manage team", plain, ActionManageTeam, tenancy.Resource{}, false},
		{"owner changes plan", owner, ActionManageBilling, tenancy.Resource{}, true},
		{"member cannot change plan", maker, ActionManageBilling, tenancy.Resource{}, false},
		{"owner deletes namespace", owner, tenancy.ActionDeleteNamespace, tenancy.Resource{}, true},
		{"member cannot delete namespace", plain, tenancy.ActionDeleteNamespace, tenancy.Resource{}, false},
		{"granted member creates namespace", maker, tenancy.ActionCreateNamespace, tenancy.Resource{}, true},
		{"ungranted member cannot", plain, tenancy.ActionCreateNamespace, tenancy.Resource{}, false},
		{"owner manages tokens anywhere", owner, tenancy.ActionManageNamespaceTokens, tenancy.Resource{Namespace: "beta"}, true},
		{"token holder manages alpha", tokener, tenancy.ActionManageNamespaceTokens, tenancy.Resource{Namespace: "alpha"}, true},
		{"token holder cannot manage beta", tokener, tenancy.ActionManageNamespaceTokens, tenancy.Resource{Namespace: "beta"}, false},
		{"unknown namespace reads as denied", tokener, tenancy.ActionManageNamespaceTokens, tenancy.Resource{Namespace: "nope"}, false},
		{"member without grant cannot", plain, tenancy.ActionManageNamespaceTokens, tenancy.Resource{Namespace: "alpha"}, false},
		{"unknown action is denied", owner, tenancy.Action("bogus"), tenancy.Resource{}, false},
	}
	for _, c := range cases {
		err := z.Authorize(context.Background(), Actor{Org: org.ID, User: c.user}, c.act, c.res)
		if (err == nil) != c.want {
			t.Errorf("%s: err = %v, want allowed=%v", c.who, err, c.want)
		}
		if err != nil && !errors.Is(err, tenancy.ErrDenied) {
			t.Errorf("%s: err = %v, want it to wrap ErrDenied", c.who, err)
		}
	}

	// Superadmin needs no org.
	if err := z.Authorize(context.Background(), Actor{User: super.ID}, ActionSuperadmin, tenancy.Resource{}); err != nil {
		t.Errorf("superadmin: %v", err)
	}
	if err := z.Authorize(context.Background(), Actor{Org: org.ID, User: owner}, ActionSuperadmin, tenancy.Resource{}); err == nil {
		t.Error("an org owner is not a superadmin")
	}
}
