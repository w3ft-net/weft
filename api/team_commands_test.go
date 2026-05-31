package api

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/store"
)

// TestMaybeHandleTeamCommand_Routing covers the interception boundary —
// which pipelines are claimed as team commands vs forwarded to the
// coordinator — without needing a store (the claimed cases here stop at
// the owner check or the parse guard).
func TestMaybeHandleTeamCommand_Routing(t *testing.T) {
	s := &Server{}
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
		{"adduser alice@x.test payments && echo hi", false, ""},
		// A non-owner trying to mutate is claimed and rejected here,
		// before any store access.
		{"adduser bob@x.test", true, "only the owner"},
		{"deluser bob@x.test payments", true, "only the owner"},
	}
	for _, c := range cases {
		handled, _, err := s.maybeHandleTeamCommand(member, c.pipeline)
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
func ownerSession(t *testing.T, s *store.Store, orgName string) (*auth.Session, *store.Customer) {
	t.Helper()
	cust, err := s.FindOrCreateCustomer(orgName)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), CustomerID: cust.ID,
		Tier: string(store.TierSwatch), Status: "active", StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := s.CreateUser(fmt.Sprintf("owner-%d@x.test", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTeamMember(&store.TeamMember{
		ID: uuid.New().String(), CustomerID: cust.ID, Email: owner.Email,
		UserID: &owner.ID, Role: "owner", Status: "active",
		InvitedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return &auth.Session{
		UserID: owner.ID, Email: owner.Email, IsOwner: true,
		ActiveCustomerID: cust.ID, ActiveCustomerName: cust.Name,
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
	out, err := srv.teamAddUser(sess, []string{newbie, "payments"})
	if err != nil {
		t.Fatalf("adduser newbie payments: %v", err)
	}
	if !strings.Contains(out, "invite") || !strings.Contains(out, "accept") {
		t.Errorf("expected invite+accept message, got %q", out)
	}
	m, err := s.GetTeamMemberByEmail(cust.ID, newbie)
	if err != nil || m == nil || m.UserID != nil || m.Status != "pending" {
		t.Fatalf("expected pending member, got %+v err=%v", m, err)
	}

	// Re-running before they accept errors clearly.
	if _, err := srv.teamAddUser(sess, []string{newbie, "payments"}); err == nil ||
		!strings.Contains(err.Error(), "hasn't accepted") {
		t.Fatalf("expected hasn't-accepted error, got %v", err)
	}

	// A typo'd group is rejected with no side effect — nobody invited.
	other := fmt.Sprintf("other-%d@x.test", os.Getpid())
	if _, err := srv.teamAddUser(sess, []string{other, "nope"}); err == nil ||
		!strings.Contains(err.Error(), "no such group") {
		t.Fatalf("expected bad-group error, got %v", err)
	}
	if mm, _ := s.GetTeamMemberByEmail(cust.ID, other); mm != nil {
		t.Errorf("typo'd group should not have invited %s", other)
	}
}
