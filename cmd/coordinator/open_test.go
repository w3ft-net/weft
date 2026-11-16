package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/cmd/coordinator/server"
	"github.com/w3ft-net/weft/cmd/coordinator/server/options"
)

// An open coordinator with no credential configured must not start: open is
// where the process reads its secrets.
func TestOpen_RefusesWithoutACredential(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "")
	t.Setenv(defaults.BasicPasswordEnv, "")
	if _, _, err := open(&server.Flags{Flags: options.Flags{DBDSN: filepath.Join(t.TempDir(), "weft.db")}}); !errors.Is(err, defaults.ErrNoCredential) {
		t.Fatalf("Open() = %v, want ErrNoCredential", err)
	}
}

func TestOpen_BuildsEveryRequiredPart(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "u")
	t.Setenv(defaults.BasicPasswordEnv, "p")
	deps, closeDB, err := open(&server.Flags{Flags: options.Flags{DBDSN: filepath.Join(t.TempDir(), "weft.db")}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if err := deps.Validate(); err != nil {
		t.Fatal(err)
	}
}
