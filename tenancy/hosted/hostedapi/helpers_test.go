package hostedapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/graybeardcorp/saaskit/orgs"
	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// productOver is the hosted api server over s, as production wires it: the
// hosted tenancy, and the product's routes on the mux.
func productOver(s *store.Store, jwtSecret string) *product {
	srv := weftapi.NewServer(s, "", "http://localhost")
	srv.SetTenancy(hosted.NewAdapter(s))
	p := newProduct()
	p.secret = jwtSecret
	p.attach(srv)
	srv.SetIdentity(p)
	srv.Extend(p.routes)
	return p
}

// testServer is productOver a rolled-back test transaction.
func testServer(t *testing.T) (*product, *store.Store) {
	t.Helper()
	s, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := s.BeginTest()
	t.Cleanup(func() {
		rollback()
		s.Close()
	})
	return productOver(tx, "test-secret"), tx
}

// superadminJWT creates a superadmin user and returns a JWT for it. Admin
// endpoints gate on session.IsSuperadmin, which AuthMiddleware materializes
// from the users row — so the flag has to be persisted, not just claimed.
func superadminJWT(t *testing.T, s *store.Store) string {
	t.Helper()
	email := fmt.Sprintf("super-%d@example.com", os.Getpid())
	user, err := s.CreateUser(email)
	if err != nil {
		t.Fatalf("create superadmin: %v", err)
	}
	if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).
		Update("is_superadmin", true).Error; err != nil {
		t.Fatalf("promote superadmin: %v", err)
	}
	user.IsSuperadmin = true

	// BuildSession (via AuthMiddleware) needs the caller to belong to at
	// least one org — a superadmin is still a real user somewhere.
	cust, err := s.FindOrCreateOrg(fmt.Sprintf("super-org-%d", os.Getpid()))
	if err != nil {
		t.Fatalf("superadmin org: %v", err)
	}
	if err := s.AddTeamMemberWithEmail(cust.ID, user.ID, email, store.RoleOwner); err != nil {
		t.Fatalf("superadmin membership: %v", err)
	}

	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}
	return jwt
}

func permsSeed(t *testing.T, s *store.Store, cust *store.Organization, role orgs.Role, emailSuffix string) (*store.User, string) {
	t.Helper()
	user, err := s.CreateVerifiedUser(fmt.Sprintf("%s-%d@example.com", emailSuffix, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, role); err != nil {
		t.Fatal(err)
	}
	jwt, _ := auth.Generate(user, "test-secret")
	return user, jwt
}

// groupedOrg is an org with an owner and three seats: none of the groups, and
// exactly one group each of cron and etc.
type groupedOrg struct {
	srv *product
	s   *store.Store
	org *store.Organization

	owner, plain, cron, etc string // JWTs
}

func newGroupedOrg(t *testing.T, tier store.Tier) *groupedOrg {
	t.Helper()
	srv, s := testServer(t)
	org, err := s.FindOrCreateOrg(fmt.Sprintf("hostedapi-restperm-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	// Every real org has a subscription row; signup creates a trial one.
	if err := s.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), OrgID: org.ID, Tier: string(tier), Status: "active", StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, ownerJWT := permsSeed(t, s, org, store.RoleOwner, "restperm-owner")
	if _, err := s.FindOrCreateNamespace(org.ID, "ops"); err != nil {
		t.Fatal(err)
	}
	g := &groupedOrg{srv: srv, s: s, org: org, owner: ownerJWT}
	grant := store.TeamPermsDoc{}
	seat := func(suffix string, p store.MemberPerms) string {
		u, jwt := permsSeed(t, s, org, orgs.RoleMember, "restperm-"+suffix)
		// Every seat reads ops, so only a missing group can refuse a write there.
		p.Namespaces = map[string]store.NamespaceAcl{"ops": {Read: true, Write: suffix == "cron"}}
		grant[u.Email] = p
		return jwt
	}
	g.plain = seat("plain", store.MemberPerms{})
	g.cron = seat("cron", store.MemberPerms{})
	g.etc = seat("etc", store.MemberPerms{Etc: store.AccessEdit})

	body, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if w := g.do(ownerJWT, "PUT", "/api/permissions", body); w.Code != 204 {
		t.Fatalf("grant permissions: %d %s", w.Code, w.Body.String())
	}
	return g
}

func (g *groupedOrg) do(jwt, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.srv.Handler().ServeHTTP(w, req)
	return w
}

// seedCompOwner makes a verified owner of a new org on a comp plan, with the
// Loom seat the operator's create-customer route gives, and returns the user.
func seedCompOwner(t *testing.T, srv *product, s *store.Store, email, orgName, tier, reason string) *store.User {
	t.Helper()
	email, orgName = store.NormalizeEmail(email), store.NormalizeOrgName(orgName)
	cust, err := s.FindOrCreateOrg(orgName)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	user, err := s.CreateVerifiedUser(email)
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := s.AddTeamMemberWithEmail(cust.ID, user.ID, email, store.RoleOwner); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := s.GrantCompSubscription(cust.ID, tier, reason); err != nil {
		t.Fatalf("seed comp: %v", err)
	}
	if err := srv.grantLoomMembership(t.Context(), user.ID, email); err != nil {
		t.Fatalf("seed loom seat: %v", err)
	}
	_ = s.UpdateUserLastActiveOrg(user.ID, cust.ID)
	return user
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%d@example.com", os.Getpid())
}

// dashTeamMember makes user an owner member of cust, so BuildSession finds an
// active org.
func dashTeamMember(s *store.Store, cust *store.Organization, user *store.User, idSuffix string) {
	_ = s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
}
