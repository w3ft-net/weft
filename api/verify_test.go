package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// These tests pin the contract of the signup and magic-link verify flows:
//
//   - a verify link can only ever create an org, never adopt an existing one;
//   - one email address is one account, whatever its case;
//   - a link proves ownership of its address and does nothing else — in
//     particular it does not accept an unrelated pending invite;
//   - nothing durable (user, org, JWT) exists before the address is verified;
//   - a verify that cannot finish leaves the link usable and no account behind.

// verifyReq posts to /api/auth/verify. A handler panic is reported as a test
// failure rather than aborting the run, since a nil dereference on a failed
// org lookup is one of the behaviors under test.
func verifyReq(t *testing.T, srv *Server, token, orgName string) (w *httptest.ResponseRecorder) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("verify panicked: %v", r)
			w = httptest.NewRecorder()
			w.Code = 500
		}
	}()
	body, _ := json.Marshal(map[string]string{"token": token, "org_name": orgName})
	req := httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func loginReq(t *testing.T, srv *Server, email string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("login: status %d: %s", w.Code, w.Body.String())
	}
}

func signupReq(t *testing.T, srv *Server, email, org string) *httptest.ResponseRecorder {
	t.Helper()
	srv.StripePrices[store.TierSwatch] = TierPrices{Flat: "price_test"}
	body, _ := json.Marshal(map[string]string{"email": email, "org_name": org, "tier": "swatch"})
	req := httptest.NewRequest("POST", "/api/auth/signup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func countUsersByEmail(t *testing.T, s *store.Store, email string) int64 {
	t.Helper()
	var n int64
	if err := s.DB().Model(&store.User{}).Where("lower(email) = lower(?)", email).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// seedOwnedOrg creates an org with one verified owner and returns both.
func seedOwnedOrg(t *testing.T, s *store.Store, slug string) (*store.Organization, *store.User) {
	t.Helper()
	name := fmt.Sprintf("%s-%d", slug, os.Getpid())
	org, err := s.FindOrCreateOrg(name)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.CreateVerifiedUser(name + "-owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMemberWithEmail(org.ID, owner.ID, owner.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	// A paid plan, so the org has seats to invite into.
	if err := s.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), OrgID: org.ID, Tier: string(store.TierYard), Status: "active", StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return org, owner
}

// C2: a first-time verify supplies org_name, which must name a new org. It
// used to be a find-or-create, so naming an existing org made the caller its
// owner.
func TestVerify_NewUserCannotAdoptExistingOrg(t *testing.T) {
	srv, s := testServer(t)
	victim, _ := seedOwnedOrg(t, s, "verify-victim")

	attacker := fmt.Sprintf("verify-attacker-%d@example.com", os.Getpid())
	token, err := s.CreateMagicLink(attacker)
	if err != nil {
		t.Fatal(err)
	}

	w := verifyReq(t, srv, token, victim.Name)

	if w.Code == 200 {
		t.Errorf("verify naming an existing org succeeded: %s", w.Body.String())
	}
	if u, err := s.GetUserByEmail(attacker); err == nil {
		if m, err := s.GetTeamMemberByUserID(victim.ID, u.ID); err == nil && m != nil {
			t.Errorf("attacker joined the victim org with role %q", m.Role)
		}
	}
}

// The org name is compared the way signup compares it: trimmed and
// lowercased. A case variant must not slip past the existing-org check.
func TestVerify_NewUserCannotAdoptExistingOrgViaCaseVariant(t *testing.T) {
	srv, s := testServer(t)
	victim, _ := seedOwnedOrg(t, s, "verify-casevictim")

	attacker := fmt.Sprintf("verify-caseattacker-%d@example.com", os.Getpid())
	token, _ := s.CreateMagicLink(attacker)

	w := verifyReq(t, srv, token, "  "+strings.ToUpper(victim.Name)+"  ")

	if w.Code == 200 {
		t.Errorf("verify naming an existing org (case variant) succeeded: %s", w.Body.String())
	}
}

// One address is one account. A link issued to a case variant of an existing
// user's address must resolve to that user, not mint a second account.
func TestVerify_CaseVariantEmailResolvesToExistingUser(t *testing.T) {
	srv, s := testServer(t)
	_, owner := seedOwnedOrg(t, s, "verify-casemail")

	variant := strings.ToUpper(owner.Email)
	token, err := s.CreateMagicLink(variant)
	if err != nil {
		t.Fatal(err)
	}

	w := verifyReq(t, srv, token, fmt.Sprintf("casemail-new-%d", os.Getpid()))

	if n := countUsersByEmail(t, s, owner.Email); n != 1 {
		t.Errorf("%d user rows for one address after verify, want 1", n)
	}
	if w.Code == 200 {
		var resp struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.User.ID != "" && resp.User.ID != owner.ID {
			t.Errorf("verify signed in as user %s, want existing user %s", resp.User.ID, owner.ID)
		}
	}
}

// A login link proves the address and nothing more. An existing user with a
// pending invite elsewhere must not be pulled into that org by logging in.
func TestVerify_LoginLinkDoesNotAcceptPendingInvite(t *testing.T) {
	srv, s := testServer(t)
	inviterOrg, inviter := seedOwnedOrg(t, s, "verify-inviter")

	invitee, err := s.CreateVerifiedUser(fmt.Sprintf("verify-invitee-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InviteTeamMember(inviterOrg.ID, invitee.Email, orgs.RoleMember, inviter.ID); err != nil {
		t.Fatal(err)
	}

	loginReq(t, srv, invitee.Email)
	token, err := s.GetLastMagicLinkToken(invitee.Email)
	if err != nil {
		t.Fatal(err)
	}

	w := verifyReq(t, srv, token, "")

	if w.Code != 200 {
		t.Fatalf("login verify: status %d: %s", w.Code, w.Body.String())
	}
	if m, err := s.GetTeamMemberByUserID(inviterOrg.ID, invitee.ID); err == nil && m != nil {
		t.Errorf("a login link accepted the pending invite into %s", inviterOrg.Name)
	}
}

// Clicking the link is proof of address ownership whatever else is pending,
// so an unverified user holding a pending invite still ends up verified.
func TestVerify_VerifiesUnverifiedUserEvenWithPendingInvite(t *testing.T) {
	srv, s := testServer(t)
	inviterOrg, inviter := seedOwnedOrg(t, s, "verify-inviter2")

	email := fmt.Sprintf("verify-unverified-%d@example.com", os.Getpid())
	user, err := s.CreateUser(email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InviteTeamMember(inviterOrg.ID, email, orgs.RoleMember, inviter.ID); err != nil {
		t.Fatal(err)
	}
	token, _ := s.CreateMagicLink(email)

	w := verifyReq(t, srv, token, "")

	if w.Code != 200 {
		t.Fatalf("verify: status %d: %s", w.Code, w.Body.String())
	}
	got, err := s.GetUserByID(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EmailVerifiedAt == nil {
		t.Error("user who proved their address is still unverified")
	}
}

// Signup must not hand out a session: the address is unproven, and the JWT
// is what turned a later invite or verify into access for whoever signed up
// first (C7).
func TestSignup_ReturnsNoSessionBeforeVerification(t *testing.T) {
	srv, _ := testServer(t)

	w := signupReq(t, srv, fmt.Sprintf("signup-nojwt-%d@example.com", os.Getpid()), fmt.Sprintf("signup-nojwt-%d", os.Getpid()))

	if w.Code >= 300 {
		t.Fatalf("signup: status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if tok, _ := resp["token"].(string); tok != "" {
		t.Error("signup returned a JWT before the address was verified")
	}
}

// Nothing durable may exist for an address nobody has proven they own:
// no org (name squatting), no user row (address squatting).
func TestSignup_CreatesNothingBeforeVerification(t *testing.T) {
	srv, s := testServer(t)
	email := fmt.Sprintf("signup-nothing-%d@example.com", os.Getpid())
	org := fmt.Sprintf("signup-nothing-%d", os.Getpid())

	w := signupReq(t, srv, email, org)

	if w.Code >= 300 {
		t.Fatalf("signup: status %d: %s", w.Code, w.Body.String())
	}
	if _, err := s.GetOrgByName(org); err == nil {
		t.Error("signup created the org before the address was verified")
	}
	if n := countUsersByEmail(t, s, email); n != 0 {
		t.Errorf("signup created %d user rows before the address was verified", n)
	}
}

// The full happy path: signup, then the emailed link, yields the org, an
// owner membership, a verified user and a session in one step.
func TestSignup_VerifyLinkCompletesAccount(t *testing.T) {
	srv, s := testServer(t)
	email := fmt.Sprintf("signup-complete-%d@example.com", os.Getpid())
	org := fmt.Sprintf("signup-complete-%d", os.Getpid())

	if w := signupReq(t, srv, email, org); w.Code >= 300 {
		t.Fatalf("signup: status %d: %s", w.Code, w.Body.String())
	}
	token, err := s.GetLastMagicLinkToken(email)
	if err != nil {
		t.Fatalf("signup sent no link: %v", err)
	}

	w := verifyReq(t, srv, token, "")

	if w.Code != 200 {
		t.Fatalf("verify: status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if tok, _ := resp["token"].(string); tok == "" {
		t.Error("verify returned no session")
	}
	user, err := s.GetUserByEmail(email)
	if err != nil {
		t.Fatalf("no user after verify: %v", err)
	}
	if user.EmailVerifiedAt == nil {
		t.Error("user is unverified after verify")
	}
	o, err := s.GetOrgByName(org)
	if err != nil {
		t.Fatalf("no org after verify: %v", err)
	}
	m, err := s.GetTeamMemberByUserID(o.ID, user.ID)
	if err != nil || m == nil || m.Role != store.RoleOwner {
		t.Errorf("signup user is not the owner of their org: member=%v err=%v", m, err)
	}
}

// A link that cannot be redeemed (here a login link for an address with no
// account) must fail cleanly: 4xx, no account conjured, and the link left as
// it was. The token used to be burned first and the create errors ignored.
func TestVerify_LoginLinkWithoutAccountCreatesNothing(t *testing.T) {
	srv, s := testServer(t)
	email := fmt.Sprintf("verify-noaccount-%d@example.com", os.Getpid())
	token, _ := s.CreateMagicLink(email)

	w := verifyReq(t, srv, token, "any-org")

	if w.Code < 400 || w.Code >= 500 {
		t.Errorf("status %d, want 4xx: %s", w.Code, w.Body.String())
	}
	if n := countUsersByEmail(t, s, email); n != 0 {
		t.Errorf("failed verify left %d user rows behind", n)
	}
	if _, err := s.PeekMagicLink(token); err != nil {
		t.Errorf("failed verify burned the link: %v", err)
	}
}

// Nothing is reserved between signup and verify, so verify must enforce the
// org name again. When it loses the race it fails with a 409 and leaves the
// link and the address untouched, so the user can retry with another name.
func TestVerify_SignupLinkFailsCleanlyWhenOrgNameTakenSinceSignup(t *testing.T) {
	srv, s := testServer(t)
	email := fmt.Sprintf("verify-race-%d@example.com", os.Getpid())
	org := fmt.Sprintf("verify-race-%d", os.Getpid())

	if w := signupReq(t, srv, email, org); w.Code >= 300 {
		t.Fatalf("signup: status %d: %s", w.Code, w.Body.String())
	}
	token, _ := s.GetLastMagicLinkToken(email)
	if _, err := s.CreateOrg(org); err != nil {
		t.Fatal(err)
	}

	w := verifyReq(t, srv, token, "")

	if w.Code != 409 {
		t.Errorf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := countUsersByEmail(t, s, email); n != 0 {
		t.Errorf("failed verify left %d user rows behind", n)
	}
	if _, err := s.PeekMagicLink(token); err != nil {
		t.Errorf("failed verify burned the link: %v", err)
	}
}

// Signup names the plan the user picked; the verify response hands it back so
// the SPA can carry on to payment now that no session exists at signup.
func TestVerify_SignupLinkReportsChosenTier(t *testing.T) {
	srv, s := testServer(t)
	email := fmt.Sprintf("verify-tier-%d@example.com", os.Getpid())
	signupReq(t, srv, email, fmt.Sprintf("verify-tier-%d", os.Getpid()))
	token, _ := s.GetLastMagicLinkToken(email)

	w := verifyReq(t, srv, token, "")

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["signup_tier"] != "swatch" {
		t.Errorf("signup_tier = %v, want swatch: %s", resp["signup_tier"], w.Body.String())
	}
}

// A login link is only ever issued for an account that exists; asking for one
// for any other address answers the same way and sends nothing.
func TestLogin_UnknownAddressIssuesNoLink(t *testing.T) {
	srv, s := testServer(t)
	email := fmt.Sprintf("login-unknown-%d@example.com", os.Getpid())

	loginReq(t, srv, email)

	if _, err := s.GetLastMagicLinkToken(email); err == nil {
		t.Error("login issued a link for an address with no account")
	}
}

// A pending invitee has no account yet and no use for a login link: their
// invite link is the way in.
func TestLogin_PendingInviteeWithoutAccountIssuesNoLink(t *testing.T) {
	srv, s := testServer(t)
	org, owner := seedOwnedOrg(t, s, "login-invitee")
	email := fmt.Sprintf("login-invitee-%d@example.com", os.Getpid())
	if _, err := s.InviteTeamMember(org.ID, email, orgs.RoleMember, owner.ID); err != nil {
		t.Fatal(err)
	}

	loginReq(t, srv, email)

	if _, err := s.GetLastMagicLinkToken(email); err == nil {
		t.Error("login issued a link for an invitee with no account")
	}
}

// --- invite links ---

func acceptReq(srv *Server, token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"token": token})
	req := httptest.NewRequest("POST", "/api/team/accept", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// invite sends a real invite through the server and returns its emailed token.
func invite(t *testing.T, srv *Server, s *store.Store, org *store.Organization, owner *store.User, email string) string {
	t.Helper()
	session := &auth.Session{
		UserID: owner.ID, Email: owner.Email,
		ActiveOrgID: org.ID, ActiveCustomerName: org.Name, IsOwner: true,
	}
	if _, err := srv.inviteMember(context.Background(), session, email); err != nil {
		t.Fatalf("invite %s: %v", email, err)
	}
	token, err := s.GetLastMagicLinkToken(email)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The invite link creates a verified account for a new invitee and joins them
// to the org that invited them.
func TestAcceptInvite_NewUserJoinsInvitingOrgVerified(t *testing.T) {
	srv, s := testServer(t)
	org, owner := seedOwnedOrg(t, s, "accept-new")
	email := fmt.Sprintf("accept-new-%d@example.com", os.Getpid())
	token := invite(t, srv, s, org, owner, email)

	w := acceptReq(srv, token)

	if w.Code != 200 {
		t.Fatalf("accept: status %d: %s", w.Code, w.Body.String())
	}
	user, err := s.GetUserByEmail(email)
	if err != nil {
		t.Fatalf("no user after accept: %v", err)
	}
	if user.EmailVerifiedAt == nil {
		t.Error("invitee who followed the link is unverified")
	}
	if m, err := s.GetTeamMemberByUserID(org.ID, user.ID); err != nil || m == nil {
		t.Errorf("invitee is not a member of the inviting org: %v", err)
	}
}

// An invite link joins the org it names and no other, even when the address
// has several pending invites.
func TestAcceptInvite_JoinsOnlyTheOrgTheLinkNames(t *testing.T) {
	srv, s := testServer(t)
	orgA, ownerA := seedOwnedOrg(t, s, "accept-a")
	orgB, ownerB := seedOwnedOrg(t, s, "accept-b")
	email := fmt.Sprintf("accept-two-%d@example.com", os.Getpid())
	tokenA := invite(t, srv, s, orgA, ownerA, email)
	_ = invite(t, srv, s, orgB, ownerB, email)

	if w := acceptReq(srv, tokenA); w.Code != 200 {
		t.Fatalf("accept: status %d: %s", w.Code, w.Body.String())
	}

	user, _ := s.GetUserByEmail(email)
	if m, err := s.GetTeamMemberByUserID(orgA.ID, user.ID); err != nil || m == nil {
		t.Errorf("not a member of the org the link named: %v", err)
	}
	if m, err := s.GetTeamMemberByUserID(orgB.ID, user.ID); err == nil && m != nil {
		t.Error("link for one org also accepted the other org's invite")
	}
}

// Links are not interchangeable between endpoints: a login link cannot accept
// an invite, and an invite link cannot sign anyone in. A refused link is not
// consumed.
func TestLinks_AreNotInterchangeableBetweenEndpoints(t *testing.T) {
	srv, s := testServer(t)
	org, owner := seedOwnedOrg(t, s, "purpose")
	email := fmt.Sprintf("purpose-%d@example.com", os.Getpid())

	inviteToken := invite(t, srv, s, org, owner, email)
	if w := verifyReq(t, srv, inviteToken, ""); w.Code != 401 {
		t.Errorf("verify accepted an invite link: status %d", w.Code)
	}
	if _, err := s.PeekMagicLink(inviteToken); err != nil {
		t.Errorf("verify consumed an invite link it refused: %v", err)
	}

	loginToken, _ := s.CreateMagicLink(owner.Email)
	if w := acceptReq(srv, loginToken); w.Code != 401 {
		t.Errorf("accept took a login link: status %d", w.Code)
	}
	if _, err := s.PeekMagicLink(loginToken); err != nil {
		t.Errorf("accept consumed a login link it refused: %v", err)
	}
}

// An invite withdrawn after its link was sent can no longer be accepted, and
// the failed attempt leaves no account behind.
func TestAcceptInvite_WithdrawnInviteFailsCleanly(t *testing.T) {
	srv, s := testServer(t)
	org, owner := seedOwnedOrg(t, s, "withdrawn")
	email := fmt.Sprintf("withdrawn-%d@example.com", os.Getpid())
	token := invite(t, srv, s, org, owner, email)
	if err := s.RemoveTeamMemberByEmail(org.ID, email); err != nil {
		t.Fatal(err)
	}

	w := acceptReq(srv, token)

	if w.Code != 404 {
		t.Errorf("status %d, want 404: %s", w.Code, w.Body.String())
	}
	if n := countUsersByEmail(t, s, email); n != 0 {
		t.Errorf("failed accept left %d user rows behind", n)
	}
}
