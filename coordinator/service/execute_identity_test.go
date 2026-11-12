package service

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// Execute decides what a query may read from who is calling, as the
// interceptor authenticated them. The request used to carry a user_id, the
// caller's own claim about itself: an empty one was the system identity,
// which reads every namespace, so any member could read them all by sending
// none. The field is gone from the protocol.

type collectStream struct {
	grpc.ServerStream
	ctx context.Context
	out strings.Builder
}

func (s *collectStream) Context() context.Context     { return s.ctx }
func (s *collectStream) SetTrailer(metadata.MD)       {}
func (s *collectStream) SetHeader(metadata.MD) error  { return nil }
func (s *collectStream) SendHeader(metadata.MD) error { return nil }
func (s *collectStream) Send(r *pb.CoordinatorExecuteResponse) error {
	for _, l := range r.Lines {
		s.out.WriteString(l.Raw + "\n")
	}
	s.out.WriteString(r.Stderr)
	return nil
}

func TestExecute_UsesTheAuthenticatedCaller(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	org, _ := st.FindOrCreateOrg("exec-identity")
	st.SeedDevSubscription(org.ID)
	if _, err := st.FindOrCreateNamespace(org.ID, "beta"); err != nil {
		t.Fatal(err)
	}
	mk := func(email string, role orgs.Role) *store.User {
		u, err := st.CreateVerifiedUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddTeamMemberWithEmail(org.ID, u.ID, u.Email, role); err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner := mk("exec-owner@example.test", store.RoleOwner)
	member := mk("exec-member@example.test", orgs.RoleMember)

	srv := &Server{Coord: coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: st}), Store: st}
	ctxFor := func(u *store.User) context.Context {
		ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: u.ID})
		return auth.WithSession(ctx, &auth.Session{UserID: u.ID, Email: u.Email, ActiveOrgID: org.ID, ActiveCustomerName: org.Name, EmailVerified: true})
	}
	run := func(caller *store.User) string {
		s := &collectStream{ctx: ctxFor(caller)}
		_ = srv.Execute(&pb.CoordinatorExecuteRequest{Pipeline: "cat /log/beta/api/main.log"}, s)
		return s.out.String()
	}

	// The request carries no user: who is asking is the authenticated caller.
	// A member with no read permission is refused, an owner is not.
	if out := run(member); !strings.Contains(out, "unknown source: beta") {
		t.Errorf("member: output %q, want a refusal", out)
	}
	if out := run(owner); strings.Contains(out, "unknown source") {
		t.Errorf("owner was refused: %q", out)
	}
}
