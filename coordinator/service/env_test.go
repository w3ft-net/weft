package service

import (
	"context"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/identity/open"
	"github.com/w3ft-net/weft/mounts"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// svcEnv is a service over the open build's stores: one SQLite database, the
// one group the default shape has. Under a shape with group columns the open
// build has no group to run as, so a test that builds one skips.
type svcEnv struct {
	t     *testing.T
	st    *mem.Stores
	owner shape.Added
}

func newSvcEnv(t *testing.T) *svcEnv {
	t.Helper()
	if coordinator.NoGroup(shape.Added{}) {
		t.Skip("the open build has no group under a shape with required columns")
	}
	st := mem.Open()
	return &svcEnv{t: t, st: st}
}

// allowAll allows every action: a test of the service is not one of permissions.
type allowAll struct{}

func (allowAll) Authorize(context.Context, identity.Principal, identity.Action, identity.Resource) error {
	return nil
}

// coord is a coordinator over the env's stores.
func (e *svcEnv) coord(cfg coordinator.CoordinatorConfig) *coordinator.Coordinator {
	if cfg.Compute == nil {
		cfg.Compute = &compute.Engine{}
	}
	cfg.Crons, cfg.Bins, cfg.Sources, cfg.Notices = e.st.Crons, e.st.Bins, e.st.Sources, e.st.Notices
	cfg.Shards = e.st.Shards
	cfg.Mounts = &mounts.Backends{
		Sources:    e.st.Sources,
		BinService: binscript.Service{Bins: e.st.Bins, Crons: e.st.Crons, Sources: e.st.Sources},
		Notify:     e.st.Notify,
		CronAuthor: cronauthor.Service{Crons: e.st.Crons, Sources: e.st.Sources, Bins: e.st.Bins},
		Tmp:        e.st.Tmp,
	}
	if cfg.Authz == nil {
		cfg.Authz = allowAll{}
	}
	if cfg.Scoper == nil {
		cfg.Scoper = open.Scoper{}
	}
	if cfg.Orgs == nil {
		cfg.Orgs = open.Orgs{}
	}
	return coordinator.NewCoordinator(cfg)
}

// server is srv over the env's token and source stores, with a coordinator
// built from cfg. srv may be nil for a bare Server.
func (e *svcEnv) server(cfg coordinator.CoordinatorConfig, srv *Server) *Server {
	if srv == nil {
		srv = &Server{}
	}
	srv.Coord = e.coord(cfg)
	srv.Tokens, srv.Sources = e.st.Tokens, e.st.Sources
	return srv
}

// agentToken is a token for namespace ns, which it creates.
func (e *svcEnv) agentToken(ns, label string) string {
	e.t.Helper()
	n, err := e.st.Sources.FindOrCreateNamespace(e.owner, ns)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _, err := e.st.Tokens.CreateToken(e.owner, n.ID, label, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

// inNamespace is the rows of namespace ns in the env's group.
func (e *svcEnv) inNamespace(ns string) shape.Set {
	c := e.owner.Coord()
	c.Namespace = shape.Value(ns)
	return shape.Set{c}
}

// callerCtx is ctx with user authenticated as the env's group.
func (e *svcEnv) callerCtx(ctx context.Context, user string) context.Context {
	return identity.WithCaller(ctx, testCaller{principal: open.Principal{User: user}, user: user})
}

type testCaller struct {
	principal identity.Principal
	user      string
}

func (c testCaller) Principal() identity.Principal { return c.principal }
func (c testCaller) UserID() string                { return c.user }

// collectStream is the server side of an Execute call that keeps the lines and
// the stderr it is sent.
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
