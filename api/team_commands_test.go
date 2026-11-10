package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// TestMaybeHandleTeamCommand_Routing covers the interception boundary —
// which pipelines are claimed as team commands vs forwarded to the
// coordinator — without needing a store (the claimed cases here stop at
// the owner check or the parse guard).
func TestMaybeHandleTeamCommand_Routing(t *testing.T) {
	s := &Server{authz: denyAll{}}
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
		{"deluser bob@x.test payments-r", true, "only the owner"},
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

// TestTeamAddUser_GroupGrantInvitesAndReportsPending covers the
// no-staging policy: granting a group to a non-member invites them
// (email goes out), re-running before they accept errors clearly, and a
// bad group invites nobody.
func TestTeamAddUser_GroupGrantInvitesAndReportsPending(t *testing.T) {
	srv, s := testServer(t)
	sess, cust := ownerSession(t, s, fmt.Sprintf("tc-%d", os.Getpid()))
	if _, err := s.FindOrCreateNamespace(cust.ID, "payments"); err != nil {
		t.Fatal(err)
	}
	newbie := fmt.Sprintf("newbie-%d@x.test", os.Getpid())

	// Granting to a non-member invites them; the grant is pending.
	out, err := srv.teamAddUser(context.Background(), sess, []string{newbie, "payments-r"})
	if err != nil {
		t.Fatalf("adduser newbie payments: %v", err)
	}
	if !strings.Contains(out, "invite") || !strings.Contains(out, "accept") {
		t.Errorf("expected invite+accept message, got %q", out)
	}
	m, err := s.GetTeamMemberByEmail(cust.ID, newbie)
	if err != nil || m == nil || m.UserID != "" || m.Status != "pending" {
		t.Fatalf("expected pending member, got %+v err=%v", m, err)
	}

	// Re-running before they accept errors clearly.
	if _, err := srv.teamAddUser(context.Background(), sess, []string{newbie, "payments-r"}); err == nil ||
		!strings.Contains(err.Error(), "hasn't accepted") {
		t.Fatalf("expected hasn't-accepted error, got %v", err)
	}

	// A typo'd group is rejected with no side effect — nobody invited.
	other := fmt.Sprintf("other-%d@x.test", os.Getpid())
	if _, err := srv.teamAddUser(context.Background(), sess, []string{other, "nope"}); err == nil ||
		!strings.Contains(err.Error(), "no such group") {
		t.Fatalf("expected bad-group error, got %v", err)
	}
	if mm, _ := s.GetTeamMemberByEmail(cust.ID, other); mm != nil {
		t.Errorf("typo'd group should not have invited %s", other)
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
	s := &Server{authz: denyAll{}}

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
	s := &Server{authz: allowUsers{"owner": true}}
	owner := &auth.Session{UserID: "owner", Email: "alice@x.test", IsOwner: true, ActiveCustomerName: "acme"}
	member := &auth.Session{UserID: "member", Email: "bob@x.test", IsOwner: false, ActiveCustomerName: "acme"}

	cases := []struct {
		name     string
		session  *auth.Session
		pipeline string
		wantOut  string // substring of output (help)
		wantErr  string // substring of error
	}{
		{"adduser --help", owner, "adduser --help", "usage: adduser <email> [<group>]", ""},
		{"adduser -h", owner, "adduser -h", "usage: adduser", ""},
		{"help is not privileged", member, "adduser --help", "usage: adduser", ""},
		{"deluser --help", owner, "deluser --help", "usage: deluser <email> [<group>]", ""},
		{"groups --help", owner, "groups --help", "usage: groups [<email>]", ""},
		{"id --help", member, "id --help", "usage: id [<email>]", ""},
		{"help after an email", owner, "adduser a@x.test --help", "usage: adduser", ""},
		{"unknown option", owner, "adduser -x", "", "unknown option -x"},
		{"unknown long option", owner, "deluser --force a@x.test", "", "unknown option --force"},
		{"not an email", owner, "adduser foo", "", `"foo" is not an email address`},
		{"not an email, deluser", owner, "deluser bob", "", `"bob" is not an email address`},
		{"not an email, id", member, "id bob", "", `"bob" is not an email address`},
		{"address list", owner, "adduser b@x.test,c@x.test", "", "is not an email address"},
		{"too many args, adduser", owner, "adduser a@x.test g extra", "", "usage: adduser"},
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
