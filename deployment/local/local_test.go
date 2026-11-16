package local

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
)

// An open deployment with no credential configured must not start: Open is
// where the process reads its secrets.
func TestOpen_RefusesWithoutACredential(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "")
	t.Setenv(defaults.BasicPasswordEnv, "")
	if _, _, err := Open(filepath.Join(t.TempDir(), "weft.db")); !errors.Is(err, defaults.ErrNoCredential) {
		t.Fatalf("Open() = %v, want ErrNoCredential", err)
	}
}

// What Open returns is a complete deployment: every part a process needs, and
// both halves of the credential.
func TestOpen_BuildsEveryRequiredPart(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "u")
	t.Setenv(defaults.BasicPasswordEnv, "p")
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
