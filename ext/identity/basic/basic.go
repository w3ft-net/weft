package basic

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

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/identity/open"
)

// Environment variables the open build reads its one credential from.
const (
	UserEnv     = "WEFT_BASIC_AUTH_USER"
	PasswordEnv = "WEFT_BASIC_AUTH_PASSWORD"
)

// ErrNoCredential is why the open build refuses to start: with no credential
// configured it would authenticate nobody, or everybody.
var ErrNoCredential = errors.New("no credential configured: set " + UserEnv + " and " + PasswordEnv + ", the HTTP Basic credential every request to the open build must carry")

// Credential is the open build's only identity: one HTTP Basic credential, held in
// the environment. Whoever presents it acts as the one principal of the one group.
// There is no sign-up, no login flow and no other user.
type Credential struct {
	User     string
	Password string
}

// CredentialFromEnv reads the credential from the environment. It fails with
// ErrNoCredential unless both are set, so a process that could not authenticate
// anyone does not start.
func CredentialFromEnv() (Credential, error) {
	b := Credential{User: os.Getenv(UserEnv), Password: os.Getenv(PasswordEnv)}
	if err := b.validate(); err != nil {
		return Credential{}, err
	}
	return b, nil
}

func (b Credential) validate() error {
	if b.User == "" || b.Password == "" {
		return ErrNoCredential
	}
	return nil
}

// Authenticate reports whether an Authorization header carries the credential.
// The comparison takes the same time whichever part is wrong.
func (b Credential) Authenticate(header string) bool {
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

// Caller is the authenticated party: the one user, in the one group.
func (b Credential) Caller() identity.Caller { return UserCaller{User: b.User} }

// UserCaller is the open build's caller.
type UserCaller struct{ User string }

func (c UserCaller) Principal() identity.Principal { return open.Principal{User: c.User} }
func (c UserCaller) UserID() string                { return c.User }

// Server is the coordinator's identity (package identity): it authenticates each
// call by the credential in the call's authorization metadata, which is what the
// api forwards for the caller it authenticated.
func (b Credential) Server() identity.Server { return basicServer{b} }

type basicServer struct{ b Credential }

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
