package magiclink

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/sqlitedb"
)

type mailbox struct {
	mu   sync.Mutex
	sent []sentMail
	ch   chan struct{}
}

type sentMail struct{ to, subject, body string }

func (m *mailbox) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	m.sent = append(m.sent, sentMail{to, subject, body})
	m.mu.Unlock()
	m.ch <- struct{}{}
	return nil
}

var secret = []byte("0123456789abcdef0123456789abcdef")

type fixture struct {
	id   *Identity
	h    http.Handler
	mail *mailbox
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := defaults.NewLoginBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO login_emails (email, created_at) VALUES ('me@example.test', 'x')`)
	mail := &mailbox{ch: make(chan struct{}, 4)}
	id, err := New(Config{Store: store, Secret: secret, BaseURL: "https://weft.example.test", Mail: mail})
	if err != nil {
		t.Fatal(err)
	}
	srv := weftapi.NewServer("", "https://weft.example.test")
	srv.SetIdentity(id)
	return &fixture{id: id, h: srv.Handler(), mail: mail}
}

func (f *fixture) post(path string, body any, bearer string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

// linkToken waits for the mail and returns the raw token in its link.
func (f *fixture) linkToken(t *testing.T) string {
	t.Helper()
	select {
	case <-f.mail.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no sign-in mail was sent")
	}
	body := f.mail.sent[len(f.mail.sent)-1].body
	i := strings.Index(body, "https://weft.example.test/verify?token=")
	if i < 0 {
		t.Fatalf("no link in %q", body)
	}
	u, _ := url.Parse(strings.Fields(body[i:])[0])
	return u.Query().Get("token")
}

func TestSignInFlow(t *testing.T) {
	f := newFixture(t)
	if w := f.post("/api/auth/login", map[string]string{"email": " Me@Example.test "}, ""); w.Code != 200 {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	raw := f.linkToken(t)

	w := f.post("/api/auth/verify", map[string]string{"token": raw}, "")
	if w.Code != 200 {
		t.Fatalf("verify = %d %s", w.Code, w.Body)
	}
	var out struct {
		Token string
		User  struct{ Email, Session string }
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Token == "" || out.User.Email != "me@example.test" || out.User.Session != "token" {
		t.Fatalf("verify body = %s", w.Body)
	}

	// The link works once.
	if w := f.post("/api/auth/verify", map[string]string{"token": raw}, ""); w.Code != 401 {
		t.Errorf("a spent link verified again: %d", w.Code)
	}

	// The session opens /api/auth/me; nothing does without it.
	r := httptest.NewRequest("GET", "/api/auth/me", nil)
	r.Header.Set("Authorization", "Bearer "+out.Token)
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "me@example.test") {
		t.Fatalf("me = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/me", nil))
	if w.Code != 401 {
		t.Errorf("me without a session = %d", w.Code)
	}
}

func TestLoginIsTheSameForAnUnknownAddressAndSendsNothing(t *testing.T) {
	f := newFixture(t)
	known := f.post("/api/auth/login", map[string]string{"email": "me@example.test"}, "")
	f.linkToken(t)
	unknown := f.post("/api/auth/login", map[string]string{"email": "stranger@example.test"}, "")
	if known.Code != unknown.Code || known.Body.String() != unknown.Body.String() {
		t.Fatalf("answers differ: %d %q vs %d %q", known.Code, known.Body, unknown.Code, unknown.Body)
	}
	select {
	case <-f.mail.ch:
		t.Fatal("mail was sent to an address that may not sign in")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSessionsThatDoNotVerifyAreRefused(t *testing.T) {
	f := newFixture(t)
	sign := func(method jwt.SigningMethod, key any, exp time.Time) string {
		s, _ := jwt.NewWithClaims(method, claims{jwt.RegisteredClaims{Subject: "me@example.test", ExpiresAt: jwt.NewNumericDate(exp)}}).SignedString(key)
		return s
	}
	for name, tok := range map[string]string{
		"another key": sign(jwt.SigningMethodHS256, []byte("another-key-another-key-another-ke"), time.Now().Add(time.Hour)),
		"expired":     sign(jwt.SigningMethodHS256, secret, time.Now().Add(-time.Hour)),
		"alg none":    sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, time.Now().Add(time.Hour)),
		"garbage":     "not.a.jwt",
	} {
		r := httptest.NewRequest("GET", "/api/auth/me", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s: me = %d, want 401", name, w.Code)
		}
	}
}

func TestCoordinatorHalfVerifiesTheSameSession(t *testing.T) {
	f := newFixture(t)
	session, _ := f.id.issue("me@example.test")
	un, _ := f.id.Server().Interceptors(nil)
	call := func(auth string) (identity.Caller, error) {
		ctx := context.Background()
		if auth != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", auth))
		}
		var got identity.Caller
		_, err := un[0](ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"}, func(ctx context.Context, _ any) (any, error) {
			got = identity.CallerFrom(ctx)
			return nil, nil
		})
		return got, err
	}
	if c, err := call("Bearer " + session); err != nil || c.UserID() != "me@example.test" {
		t.Fatalf("valid session: %v, %v", c, err)
	}
	if _, err := call(""); err == nil {
		t.Error("a call without a session was admitted")
	}
	if _, err := call("Bearer nope"); err == nil {
		t.Error("a bad session was admitted")
	}
}

func TestNewRefusesAWeakConfig(t *testing.T) {
	if _, err := New(Config{Secret: []byte("short")}); err == nil {
		t.Fatal("a short secret was accepted")
	}
}
