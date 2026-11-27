package local

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/ext/identity/basic"
)

// An open deployment with no credential configured must not start: Open is
// where the process reads its secrets.
func TestOpen_RefusesWithoutACredential(t *testing.T) {
	t.Setenv(basic.UserEnv, "")
	t.Setenv(basic.PasswordEnv, "")
	if _, _, err := Open(filepath.Join(t.TempDir(), "weft.db")); !errors.Is(err, basic.ErrNoCredential) {
		t.Fatalf("Open() = %v, want ErrNoCredential", err)
	}
}

// What Open returns is a complete deployment: every part a process needs, and
// both halves of the credential.
func TestOpen_BuildsEveryRequiredPart(t *testing.T) {
	t.Setenv(basic.UserEnv, "u")
	t.Setenv(basic.PasswordEnv, "p")
	l, closeOpen, err := Open(filepath.Join(t.TempDir(), "weft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpen()
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	if l.Authn.API == nil || l.Authn.GRPC == nil {
		t.Errorf("Authn = %+v, want both halves of the credential", l.Authn)
	}
	if l.Stores == nil || l.History == nil {
		t.Error("the coordinator's own stores and history are missing")
	}
}

func TestOpen_MagicLinkIdentity(t *testing.T) {
	t.Setenv(AuthEnv, "magiclink")
	t.Setenv("WEFT_SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("EMAIL_HOST", "smtp.example.test")
	t.Setenv("EMAIL_HOST_PASSWORD", "x")

	api, closeAPI, err := Open(filepath.Join(t.TempDir(), "weft.db"), WithBaseURL("https://weft.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI()
	if api.Authn.API == nil || api.Authn.GRPC == nil {
		t.Fatal("the api's deployment lacks a half of the identity")
	}
	coord, closeCoord, err := Open(filepath.Join(t.TempDir(), "weft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeCoord()
	if coord.Authn.API != nil || coord.Authn.GRPC == nil {
		t.Fatal("the coordinator verifies sessions and offers no sign-in")
	}

	t.Setenv("WEFT_SESSION_SECRET", "short")
	if _, _, err := Open(filepath.Join(t.TempDir(), "weft.db")); err == nil {
		t.Fatal("started with a weak session key")
	}
	t.Setenv(AuthEnv, "kerberos")
	if _, _, err := Open(filepath.Join(t.TempDir(), "weft.db")); err == nil {
		t.Fatal("started with an unknown identity")
	}
}
