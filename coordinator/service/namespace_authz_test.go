package service

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// AddSource, RemoveSource and CreateToken register data ingestion for a
// namespace, so they take the same permission the REST endpoints do: org owner
// or the namespace's token permission. They used to take any authenticated
// caller.
type nsFixture struct {
	srv *Server
	st  *store.Store
	org *store.Organization
	ns  *store.Namespace

	users int
}

func newNSFixture(t *testing.T) (*nsFixture, func()) {
	t.Helper()
	st, cleanup := testStore(t)
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: st})
	org, err := st.FindOrCreateOrg("nsauthz-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := st.FindOrCreateNamespace(org.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	return &nsFixture{srv: &Server{Coord: coord, Store: st}, st: st, org: org, ns: ns}, cleanup
}

func (f *nsFixture) ctx(t *testing.T, owner, tokenPerm bool) context.Context {
	t.Helper()
	f.users++
	u, err := f.st.CreateVerifiedUser(fmt.Sprintf("nsauthz-%d@example.test", f.users))
	if err != nil {
		t.Fatal(err)
	}
	if tokenPerm {
		if err := f.st.UpsertNamespacePermission(&store.NamespacePermission{UserID: u.ID, NamespaceID: f.ns.ID, CanRead: true, CanCreateTokens: true}); err != nil {
			t.Fatal(err)
		}
	}
	if owner {
		if err := f.st.AddTeamMemberWithEmail(f.org.ID, u.ID, u.Email, store.RoleOwner); err != nil {
			t.Fatal(err)
		}
	}
	return auth.WithSession(context.Background(), &auth.Session{
		UserID: u.ID, IsOwner: owner, ActiveOrgID: f.org.ID, ActiveCustomerName: f.org.Name, EmailVerified: true,
	})
}

func TestNamespaceRPCs_RequireOwnerOrTokenPermission(t *testing.T) {
	f, cleanup := newNSFixture(t)
	defer cleanup()

	add := &pb.AddSourceRequest{Namespace: "ops", Service: "api", LogGroup: "/aws/lambda/api", Region: "us-east-2"}
	calls := map[string]func(ctx context.Context) error{
		"AddSource": func(ctx context.Context) error { _, err := f.srv.AddSource(ctx, add); return err },
		"RemoveSource": func(ctx context.Context) error {
			_, err := f.srv.RemoveSource(ctx, &pb.RemoveSourceRequest{Namespace: "ops", Service: "api"})
			return err
		},
		"CreateToken": func(ctx context.Context) error {
			_, err := f.srv.CreateToken(ctx, &pb.CreateTokenRequest{Namespace: "ops", Label: "t"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name+"/plain member is denied", func(t *testing.T) {
			err := call(f.ctx(t, false, false))
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("err = %v, want PermissionDenied", err)
			}
		})
	}
	if lg, _ := f.st.FindLogGroup(f.org.ID, "ops", "api"); lg != nil {
		t.Error("a denied AddSource still registered the source")
	}

	for name, call := range calls {
		for who, ctx := range map[string]context.Context{
			"owner":                  f.ctx(t, true, false),
			"member with token perm": f.ctx(t, false, true),
		} {
			t.Run(name+"/"+who+" is allowed", func(t *testing.T) {
				if err := call(ctx); status.Code(err) == codes.PermissionDenied {
					t.Errorf("err = %v, want it allowed", err)
				}
			})
		}
	}
}
