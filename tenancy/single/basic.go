package single

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/identity"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// Environment variables the open build reads its one credential from.
const (
	BasicUserEnv     = "WEFT_BASIC_AUTH_USER"
	BasicPasswordEnv = "WEFT_BASIC_AUTH_PASSWORD"
)

// ErrNoCredential is why the open build refuses to start: with no credential
// configured it would authenticate nobody, or everybody.
var ErrNoCredential = errors.New("no credential configured: set " + BasicUserEnv + " and " + BasicPasswordEnv + ", the HTTP Basic credential every request to the open build must carry")

// Basic is the open build's only identity: one HTTP Basic credential, held in
// the environment. Whoever presents it acts as the one actor of the one tenant.
// There is no sign-up, no login flow and no other user.
type Basic struct {
	User     string
	Password string
}

// BasicFromEnv reads the credential from the environment. It fails with
// ErrNoCredential unless both are set, so a process that could not authenticate
// anyone does not start.
func BasicFromEnv() (Basic, error) {
	b := Basic{User: os.Getenv(BasicUserEnv), Password: os.Getenv(BasicPasswordEnv)}
	if err := b.validate(); err != nil {
		return Basic{}, err
	}
	return b, nil
}

func (b Basic) validate() error {
	if b.User == "" || b.Password == "" {
		return ErrNoCredential
	}
	return nil
}

// Authenticate reports whether an Authorization header carries the credential.
// The comparison takes the same time whichever part is wrong.
func (b Basic) Authenticate(header string) bool {
	if b.validate() != nil {
		return false
	}
	raw, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(dec), ":")
	if !ok {
		return false
	}
	u := subtle.ConstantTimeCompare([]byte(user), []byte(b.User))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(b.Password))
	return u&p == 1
}

// Caller is the authenticated party: the one user, in the one tenant.
func (b Basic) Caller() identity.Caller { return Caller{User: b.User} }

// Caller is the open build's caller.
type Caller struct{ User string }

func (Caller) Org() store.OrgID       { return 0 }
func (c Caller) Actor() tenancy.Actor { return Actor{User: c.User} }
func (c Caller) UserID() string       { return c.User }

// Open is the coordinator's identity (package identity): it authenticates each
// call by the credential in the call's authorization metadata, which is what the
// api forwards for the caller it authenticated.
func (b Basic) Open(*store.Store) identity.Server { return basicServer{b} }

type basicServer struct{ b Basic }

func (s basicServer) check(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")
	if len(v) == 0 || !s.b.Authenticate(v[0]) {
		return nil, status.Error(codes.Unauthenticated, "missing or wrong credential")
	}
	return identity.WithCaller(ctx, s.b.Caller()), nil
}

func (s basicServer) Interceptors(skip identity.SkipMethods) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if skip[info.FullMethod] {
			return h(ctx, req)
		}
		ctx, err := s.check(ctx)
		if err != nil {
			return nil, err
		}
		return h(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if skip[info.FullMethod] {
			return h(srv, ss)
		}
		ctx, err := s.check(ss.Context())
		if err != nil {
			return err
		}
		return h(srv, &ctxStream{ServerStream: ss, ctx: ctx})
	}
	return []grpc.UnaryServerInterceptor{unary}, []grpc.StreamServerInterceptor{stream}
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }
