// Package magiclink is an api and coordinator identity where people sign in with
// a link emailed to them. It is for a single-tenant install: who may sign in is
// the store's list of emails (backends/login), there is no sign-up, and everyone
// who signs in acts as the one group.
//
// The browser asks for a link (POST /api/auth/login), follows it to the web
// app's /verify page, which spends it (POST /api/auth/verify) for a session, and
// sends that session as a bearer token. The session is an HS256 JWT signed with
// WEFT_SESSION_SECRET, so no session is stored; the api and the coordinator
// verify it the same way, from the one secret.
package magiclink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/login"
	"github.com/w3ft-net/weft/identity"
)

// SecretEnv names the environment variable that holds the key sessions are
// signed with: at least MinSecretBytes, and the same on the api and the
// coordinator.
const SecretEnv = "WEFT_SESSION_SECRET"

// MinSecretBytes is the shortest session key accepted.
const MinSecretBytes = 32

// Defaults for how long a link and a session last.
const (
	DefaultLinkTTL    = 15 * time.Minute
	DefaultSessionTTL = 30 * 24 * time.Hour
)

// Mailer delivers a sign-in link.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Config is what an Identity is made of.
type Config struct {
	Store login.Store
	// Secret signs and verifies sessions; at least MinSecretBytes.
	Secret []byte
	// BaseURL is the web app's origin, which the emailed link points at.
	BaseURL string
	Mail    Mailer
	// LinkTTL and SessionTTL default to DefaultLinkTTL and DefaultSessionTTL.
	LinkTTL, SessionTTL time.Duration
}

// Identity is both halves of the sign-in: the api's (weftapi.Identity) and, from
// Server, the coordinator's.
type Identity struct {
	cfg Config
	now func() time.Time
}

var _ weftapi.Identity = (*Identity)(nil)

// New checks cfg and returns the identity over it.
func New(cfg Config) (*Identity, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("magiclink: no login store")
	case len(cfg.Secret) < MinSecretBytes:
		return nil, fmt.Errorf("magiclink: %s must be at least %d bytes", SecretEnv, MinSecretBytes)
	case cfg.BaseURL == "":
		return nil, errors.New("magiclink: no base URL for the sign-in link")
	case cfg.Mail == nil:
		return nil, errors.New("magiclink: no mailer: set EMAIL_HOST and EMAIL_HOST_PASSWORD to send sign-in links")
	}
	if cfg.LinkTTL <= 0 {
		cfg.LinkTTL = DefaultLinkTTL
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	return &Identity{cfg: cfg, now: time.Now}, nil
}

// Verifier is the coordinator's half on its own: it needs only the session key,
// not the store, the mail or the web app's address the api's half sends links with.
func Verifier(secret []byte) (identity.Server, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("magiclink: %s must be at least %d bytes", SecretEnv, MinSecretBytes)
	}
	return server{&Identity{cfg: Config{Secret: secret}, now: time.Now}}, nil
}

// SecretFromEnv is the session key in the environment, or an error saying how
// to set it.
func SecretFromEnv() ([]byte, error) {
	s := os.Getenv(SecretEnv)
	if len(s) < MinSecretBytes {
		return nil, fmt.Errorf("set %s to a random string of at least %d characters: the key sessions are signed with, the same on the api and the coordinator", SecretEnv, MinSecretBytes)
	}
	return []byte(s), nil
}

type claims struct {
	jwt.RegisteredClaims
}

func (i *Identity) issue(email string) (string, error) {
	now := i.now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims{jwt.RegisteredClaims{
		Subject: email, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(i.cfg.SessionTTL)),
	}}).SignedString(i.cfg.Secret)
}

// emailOf is the address a bearer session names, or ok false for anything that
// does not verify: another key, another algorithm, an expired or malformed token.
func (i *Identity) emailOf(header string) (string, bool) {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	var c claims
	tok, err := jwt.ParseWithClaims(strings.TrimSpace(raw), &c, func(*jwt.Token) (any, error) { return i.cfg.Secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(i.now), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid || c.Subject == "" {
		return "", false
	}
	return c.Subject, true
}

type caller struct{ email string }

func (c caller) Principal() identity.Principal { return defaults.OpenPrincipal{User: c.email} }
func (c caller) UserID() string                { return c.email }

// Authenticate admits a request with a valid session.
func (i *Identity) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, ok := i.emailOf(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		weftapi.RecordAccessIdentity(r, email)
		next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), caller{email})))
	})
}

// Protect has nothing to add: a session is the whole gate.
func (*Identity) Protect(next http.Handler) http.Handler { return next }

// Entitled is always true: there is no plan.
func (*Identity) Entitled(context.Context, weftapi.Session) error { return nil }

// Forward carries the session to the coordinator, which verifies it itself.
func (*Identity) Forward(r *http.Request) map[string]string {
	if h := r.Header.Get("Authorization"); h != "" {
		return map[string]string{"authorization": h}
	}
	return nil
}

type userJSON struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Customer string `json:"customer"`
	Session  string `json:"session"`
}

func user(email string) userJSON { return userJSON{ID: email, Email: email, Session: "token"} }

// Routes registers the sign-in: login and verify are public (and rate limited as
// logins are), me answers for the session.
func (i *Identity) Routes(rt *weftapi.Routes) {
	rt.Public("POST /api/auth/login", rt.Limit(weftapi.LimitLogin, http.HandlerFunc(i.handleLogin)))
	rt.Public("POST /api/auth/verify", rt.Limit(weftapi.LimitLogin, http.HandlerFunc(i.handleVerify)))
	rt.Authed("GET /api/auth/me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		weftapi.WriteJSON(w, user(weftapi.GetSession(r).UserID()))
	}))
}

// loginReply is the same whether or not the address may sign in, so the route
// does not say who has an account.
const loginReply = "If that address may sign in, a link is on its way."

func (i *Identity) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Email) == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}
	email := login.Normalize(req.Email)
	// Everything past this point runs after the reply, so how long the answer
	// takes does not tell an allowed address from another.
	go i.sendLink(email)
	weftapi.WriteJSON(w, map[string]string{"message": loginReply})
}

func (i *Identity) sendLink(email string) {
	ctx := context.Background()
	ok, err := i.cfg.Store.Allowed(email)
	if err != nil {
		weftsdk.Errorf(ctx, "magiclink: allowed check: %v", err)
		return
	}
	if !ok {
		return
	}
	raw, err := i.cfg.Store.IssueLink(email, i.cfg.LinkTTL)
	if err != nil {
		weftsdk.Errorf(ctx, "magiclink: issue link: %v", err)
		return
	}
	link := strings.TrimSuffix(i.cfg.BaseURL, "/") + "/verify?token=" + url.QueryEscape(raw)
	body := fmt.Sprintf("Follow this link to sign in to weft:\n\n%s\n\nIt works once and expires in %s. If you did not ask for it, ignore this email.\n", link, i.cfg.LinkTTL.Round(time.Minute))
	if err := i.cfg.Mail.Send(ctx, email, "Your weft sign-in link", body); err != nil {
		weftsdk.Errorf(ctx, "magiclink: send link to %s: %v", email, err)
	}
}

func (i *Identity) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	email, ok, err := i.cfg.Store.ConsumeLink(req.Token)
	if err != nil {
		http.Error(w, "verify failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "this link is invalid, expired or already used", http.StatusUnauthorized)
		return
	}
	session, err := i.issue(email)
	if err != nil {
		http.Error(w, "verify failed", http.StatusInternalServerError)
		return
	}
	weftapi.WriteJSON(w, map[string]any{"token": session, "user": user(email)})
}

// Server is the coordinator's half: it authenticates each gRPC call by the
// session in its authorization metadata, which is what Forward sends.
func (i *Identity) Server() identity.Server { return server{i} }

type server struct{ i *Identity }

func (s server) check(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")
	if len(v) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing session")
	}
	email, ok := s.i.emailOf(v[0])
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired session")
	}
	return identity.WithCaller(ctx, caller{email}), nil
}

func (s server) Interceptors(skip identity.SkipMethods) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
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
