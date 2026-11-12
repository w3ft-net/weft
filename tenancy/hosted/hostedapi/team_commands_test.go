package hostedapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/graybeardcorp/saaskit/orgs"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// TestMaybeHandleTeamCommand_Routing covers the interception boundary —
// which pipelines are claimed as team commands vs forwarded to the
// coordinator — without needing a store (the claimed cases here stop at
// the owner check or the parse guard).
func TestMaybeHandleTeamCommand_Routing(t *testing.T) {
	s := productWith(denyAll{})
	member := &auth.Session{Email: "alice@x.test", IsOwner: false, ActiveCustomerName: "acme"}

	cases := []struct {
		pipeline    string
		wantHandled bool
		wantErrSub  string // substring expected in err when handled
	}{
		{"cat api.log | grep ERROR", false, ""},
		{"range 1d..", false, ""},
		{"", false, ""},
		// Team commands inside shell plumbing fall through to the
		// normal pipeline path rather than being claimed.
		{"id | wc -l", false, ""},
		{"adduser alice@x.test payments-r && echo hi", false, ""},
		// A non-owner trying to mutate is claimed and rejected here,
		// before any store access.
		{"adduser bob@x.test", true, "only the owner"},
		{"usermod -rG payments-r bob@x.test", true, "only the owner"},
		{"deluser bob@x.test", true, "only the owner"},
	}
	for _, c := range cases {
		handled, _, err := s.maybeHandleTeamCommand(context.Background(), member, c.pipeline)
		if handled != c.wantHandled {
			t.Errorf("%q: handled=%v want %v", c.pipeline, handled, c.wantHandled)
			continue
		}
		if c.wantErrSub != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErrSub) {
				t.Errorf("%q: err=%v want substring %q", c.pipeline, err, c.wantErrSub)
			}
		}
	}
}

// ownerSession sets up a paid org (trial caps at 1 seat, which blocks
// invites) with an active owner and returns an owner session.
func ownerSession(t *testing.T, s *store.Store, orgName string) (*auth.Session, *store.Organization) {
	t.Helper()
	cust, err := s.FindOrCreateOrg(orgName)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), OrgID: cust.ID,
		Tier: string(store.TierSwatch), Status: "active", StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := s.CreateUser(fmt.Sprintf("owner-%d@x.test", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMemberWithEmail(cust.ID, owner.ID, owner.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	return &auth.Session{
		UserID: owner.ID, Email: owner.Email, IsOwner: true,
		ActiveOrgID: cust.ID, ActiveCustomerName: cust.Name,
	}, cust
}

// usermod needs an active member: granting a group to a stranger or to someone
// who hasn't accepted says what to do first, and a bad group changes nothing.
func TestTeamUsermod_MemberGroups(t *testing.T) {
	srv, s := testServer(t)
	sess, cust := ownerSession(t, s, fmt.Sprintf("tc-%d", os.Getpid()))
	if _, err := s.FindOrCreateNamespace(cust.ID, "payments"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newbie := fmt.Sprintf("newbie-%d@x.test", os.Getpid())

	if _, err := srv.teamUsermod(ctx, sess, []string{"-aG", "payments-r", newbie}); err == nil || !strings.Contains(err.Error(), "invite them first") {
		t.Fatalf("a stranger: err = %v, want a pointer to adduser", err)
	}
	if _, err := srv.teamAddUser(ctx, sess, []string{newbie}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.teamUsermod(ctx, sess, []string{"-aG", "payments-r", newbie}); err == nil || !strings.Contains(err.Error(), "hasn't accepted") {
		t.Fatalf("an invitee: err = %v, want hasn't-accepted", err)
	}
	if _, err := srv.teamUsermod(ctx, sess, []string{"-aG", "payments", sess.Email}); err == nil || !strings.Contains(err.Error(), "no such group") {
		t.Fatalf("a bare namespace name is not a group: err = %v", err)
	}
	if out, err := srv.teamUsermod(ctx, sess, []string{"-aG", "payments-w", sess.Email}); err != nil || !strings.Contains(out, "Added") {
		t.Fatalf("owner adds payments-w: %q, %v", out, err)
	}
	if out, err := srv.teamUsermod(ctx, sess, []string{"-rG", "payments-w", sess.Email}); err != nil || !strings.Contains(out, "Removed") {
		t.Fatalf("owner removes payments-w: %q, %v", out, err)
	}
}

// A namespace's crons read another namespace only once its cron user is added
// to that namespace's read group, and you can only make that grant if you hold
// write on the first namespace and read on the second.
func TestTeamUsermod_CronUser(t *testing.T) {
	srv, s := testServer(t)
	owner, cust := ownerSession(t, s, fmt.Sprintf("tc-cron-%d", os.Getpid()))
	alpha, _ := s.FindOrCreateNamespace(cust.ID, "alpha")
	beta, _ := s.FindOrCreateNamespace(cust.ID, "beta")
	ctx := context.Background()

	u, err := s.CreateVerifiedUser(fmt.Sprintf("ops-%d@x.test", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMemberWithEmail(cust.ID, u.ID, u.Email, orgs.RoleMember); err != nil {
		t.Fatal(err)
	}
	member := &auth.Session{UserID: u.ID, Email: u.Email, ActiveOrgID: cust.ID, ActiveCustomerName: cust.Name}

	// Writes alpha, reads alpha only.
	if err := s.UpsertNamespacePermission(&store.NamespacePermission{UserID: u.ID, NamespaceID: alpha.ID, CanRead: true, CanWrite: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.teamUsermod(ctx, member, []string{"-aG", "beta-r", "alpha-cron"}); err == nil || !strings.Contains(err.Error(), "you need alpha-w and beta-r") {
		t.Fatalf("without read on beta: err = %v", err)
	}

	// Reads beta too: now they hold what they hand out.
	if err := s.UpsertNamespacePermission(&store.NamespacePermission{UserID: u.ID, NamespaceID: beta.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}
	if out, err := srv.teamUsermod(ctx, member, []string{"-aG", "beta-r", "alpha-cron"}); err != nil || !strings.Contains(out, "Added") {
		t.Fatalf("with both: %q, %v", out, err)
	}
	out, err := srv.teamGroupsCmd(owner, []string{"alpha-cron"})
	if err != nil || !strings.Contains(out, "alpha-r") || !strings.Contains(out, "beta-r") {
		t.Errorf("groups alpha-cron = %q, %v", out, err)
	}
	if _, err := srv.teamUsermod(ctx, member, []string{"-aG", "beta-w", "alpha-cron"}); err == nil || !strings.Contains(err.Error(), "read group") {
		t.Errorf("a write group for a cron user: err = %v", err)
	}
	if _, err := srv.teamUsermod(ctx, member, []string{"-aG", "alpha-r", "beta-cron"}); err == nil {
		t.Error("writing alpha doesn't let you change beta's cron user")
	}
	if out, err := srv.teamUsermod(ctx, member, []string{"-rG", "beta-r", "alpha-cron"}); err != nil || !strings.Contains(out, "Removed") {
		t.Fatalf("removing: %q, %v", out, err)
	}
	if _, err := srv.teamUsermod(ctx, owner, []string{"-aG", "beta-r", "nope-cron"}); err == nil || !strings.Contains(err.Error(), "no such user") {
		t.Errorf("a namespace that doesn't exist: err = %v", err)
	}
}

// TestTeamCommands_NonOwnerDenied pins that team mutation (adduser /
// deluser) is owner-only. This is where the "who can manage the team"
// permission test lives now: team membership used to be edited as the
// virtual /etc/team.yaml file (gated in shellfs), but that file was
// removed — the team is mutated through these commands instead, so the
// gate moved to the api layer.
//
// Holding the /etc write bit never confers team management: the hosted
// Authorizer's manage-team rule is owner-only (tenancy/hosted tests). The
// check is the first statement in each handler, so a denial needs no store.
func TestTeamCommands_NonOwnerDenied(t *testing.T) {
	s := productWith(denyAll{})

	nonOwners := []struct {
		name    string
		session *auth.Session
	}{
		{"member-no-flags", &auth.Session{Email: "m@x.test", IsOwner: false}},
	}
	for _, sc := range nonOwners {
		for _, cmd := range []string{"adduser bob@x.test", "deluser bob@x.test"} {
			t.Run(sc.name+"/"+strings.Fields(cmd)[0], func(t *testing.T) {
				handled, _, err := s.maybeHandleTeamCommand(context.Background(), sc.session, cmd)
				if !handled {
					t.Fatalf("%q should be claimed as a team command", cmd)
				}
				if err == nil || !strings.Contains(err.Error(), "only the owner") {
					t.Errorf("non-owner (%s) was not denied; err=%v", sc.name, err)
				}
			})
		}
	}
}

// TestTeamCommands_OwnerPassesGate confirms the owner clears the
// owner-only gate (the positive half of the transferred /etc/team.yaml
// test). Uses deluser of a non-member: an owner gets past the IsOwner
// check and reaches RemoveMember, which reports the missing member —
// proving the gate let the owner through (a denied caller would never
// reach the store and would say "only the owner" instead).
func TestTeamCommands_OwnerPassesGate(t *testing.T) {
	srv, s := testServer(t)
	sess, _ := ownerSession(t, s, fmt.Sprintf("tc-owner-%d", os.Getpid()))

	_, err := srv.teamDelUser(context.Background(), sess, []string{fmt.Sprintf("ghost-%d@x.test", os.Getpid())})
	if err != nil && strings.Contains(err.Error(), "only the owner") {
		t.Fatalf("owner was blocked by the owner-only gate: %v", err)
	}
}

// Arguments are checked before anything runs. `adduser --help` used to be
// treated as an email address and created an invite (and sent an email)
// for a user named "--help"; deluser, groups and id passed args[0] to the
// store unchecked the same way. None of these cases may reach the store:
// the server here has none.
func TestTeamCommands_ArgumentChecks(t *testing.T) {
	s := productWith(allowUsers{"owner": true})
	owner := &auth.Session{UserID: "owner", Email: "alice@x.test", IsOwner: true, ActiveCustomerName: "acme"}
	member := &auth.Session{UserID: "member", Email: "bob@x.test", IsOwner: false, ActiveCustomerName: "acme"}

	cases := []struct {
		name     string
		session  *auth.Session
		pipeline string
		wantOut  string // substring of output (help)
		wantErr  string // substring of error
	}{
		{"adduser --help", owner, "adduser --help", "usage: adduser <email>", ""},
		{"adduser -h", owner, "adduser -h", "usage: adduser", ""},
		{"help is not privileged", member, "adduser --help", "usage: adduser", ""},
		{"deluser --help", owner, "deluser --help", "usage: deluser <email>", ""},
		{"groups --help", owner, "groups --help", "usage: groups [<user>]", ""},
		{"id --help", member, "id --help", "usage: id [<user>]", ""},
		{"help after an email", owner, "adduser a@x.test --help", "usage: adduser", ""},
		{"unknown option", owner, "adduser -x", "", "unknown option -x"},
		{"unknown long option", owner, "deluser --force a@x.test", "", "unknown option --force"},
		{"not an email", owner, "adduser foo", "", `"foo" is not an email address`},
		{"not an email, deluser", owner, "deluser bob", "", `"bob" is not an email address`},
		{"not an email, id", member, "id bob", "", `"bob" is not an email address`},
		{"address list", owner, "adduser b@x.test,c@x.test", "", "is not an email address"},
		{"too many args, adduser", owner, "adduser a@x.test g", "", "usage: adduser"},
		{"usermod --help", owner, "usermod --help", "usermod -aG <group> <user>", ""},
		{"usermod needs a flag", owner, "usermod payments-r a@x.test", "", "usage: usermod"},
		{"usermod bad flag", owner, "usermod -x payments-r a@x.test", "", "unknown option -x"},
		{"usermod user must be an email or a cron user", owner, "usermod -aG payments-r bob", "", `"bob" is not an email address`},
		{"too many args, id", member, "id a@x.test b@x.test", "", "usage: id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handled, out, err := s.maybeHandleTeamCommand(context.Background(), c.session, c.pipeline)
			if !handled {
				t.Fatalf("%q was not claimed as a team command", c.pipeline)
			}
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("%q: err = %v, want substring %q", c.pipeline, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: unexpected error %v", c.pipeline, err)
			}
			if !strings.Contains(out, c.wantOut) {
				t.Errorf("%q: output %q, want substring %q", c.pipeline, out, c.wantOut)
			}
		})
	}
}

// The team commands reach the shell through the api's command seam, so a
// pipeline sent to /api/shell/exec is answered here and never forwarded to the
// coordinator (the server in this test has none).
func TestTeamCommands_AnswerThroughShellExec(t *testing.T) {
	srv, s := testServer(t)
	cust, err := s.FindOrCreateOrg(fmt.Sprintf("tc-shell-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), OrgID: cust.ID, Tier: string(store.TierSwatch), Status: "active", StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	user, jwt := permsSeed(t, s, cust, store.RoleOwner, "tc-shell")

	req := httptest.NewRequest("POST", "/api/shell/exec", strings.NewReader(`{"pipeline":"id","range":"1h.."}`))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 200 || !strings.Contains(w.Body.String(), "uid="+user.Email) {
		t.Fatalf("id through shell exec = %d %q, want a uid line for %s", w.Code, w.Body.String(), user.Email)
	}
}
