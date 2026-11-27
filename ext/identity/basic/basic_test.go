package basic

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/identity"
)

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// The open build refuses to start without its credential: it would otherwise
// authenticate nobody, or everybody.
func TestFromEnv_RefusesWithoutACredential(t *testing.T) {
	for _, c := range []struct{ user, pass string }{{"", ""}, {"u", ""}, {"", "p"}} {
		t.Setenv(UserEnv, c.user)
		t.Setenv(PasswordEnv, c.pass)
		if _, err := CredentialFromEnv(); !errors.Is(err, ErrNoCredential) {
			t.Errorf("user=%q pass=%q: err = %v, want ErrNoCredential", c.user, c.pass, err)
		}
	}
	t.Setenv(UserEnv, "u")
	t.Setenv(PasswordEnv, "p")
	if b, err := CredentialFromEnv(); err != nil || b.User != "u" {
		t.Errorf("configured: %+v, %v", b, err)
	}
}

func TestBasic_Authenticate(t *testing.T) {
	b := Credential{User: "weft", Password: "s3cret:with:colons"}
	for _, c := range []struct {
		name, header string
		want         bool
	}{
		{"right", basicHeader("weft", "s3cret:with:colons"), true},
		{"wrong password", basicHeader("weft", "nope"), false},
		{"wrong user", basicHeader("root", "s3cret:with:colons"), false},
		{"empty", "", false},
		{"bearer", "Bearer abc", false},
		{"not base64", "Basic !!!", false},
		{"no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("weft")), false},
	} {
		if got := b.Authenticate(c.header); got != c.want {
			t.Errorf("%s: Authenticate = %v, want %v", c.name, got, c.want)
		}
	}
	// A zero credential authenticates nobody, not everybody.
	if (Credential{}).Authenticate(basicHeader("", "")) {
		t.Error("an unconfigured Credential accepted an empty credential")
	}
}

// The coordinator's interceptor takes the credential the api forwarded and
// binds the caller; a call without it never reaches the handler.
func TestBasic_CoordinatorInterceptor(t *testing.T) {
	b := Credential{User: "weft", Password: "pw"}
	unary, _ := b.Server().Interceptors(identity.SkipMethods{"/skipped": true})
	handler := func(ctx context.Context, _ any) (any, error) {
		c := identity.CallerFrom(ctx)
		if c == nil || c.UserID() != "weft" {
			t.Errorf("caller = %v", c)
		}
		return "ok", nil
	}
	call := func(method, header string) error {
		ctx := context.Background()
		if header != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", header))
		}
		_, err := unary[0](ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		return err
	}
	if err := call("/svc/m", basicHeader("weft", "pw")); err != nil {
		t.Errorf("right credential: %v", err)
	}
	for _, h := range []string{"", basicHeader("weft", "x")} {
		if err := call("/svc/m", h); status.Code(err) != codes.Unauthenticated {
			t.Errorf("header %q: err = %v, want Unauthenticated", h, err)
		}
	}
	// A skipped method does its own authentication.
	skipHandler := func(context.Context, any) (any, error) { return "ok", nil }
	if _, err := unary[0](context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/skipped"}, skipHandler); err != nil {
		t.Errorf("skipped method: %v", err)
	}
}
