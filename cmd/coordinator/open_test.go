package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
)

// An open coordinator with no credential configured must not start: open is
// where the process reads its secrets.
func TestOpen_RefusesWithoutACredential(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "")
	t.Setenv(defaults.BasicPasswordEnv, "")
	if _, _, err := open(filepath.Join(t.TempDir(), "weft.db")); !errors.Is(err, defaults.ErrNoCredential) {
		t.Fatalf("Open() = %v, want ErrNoCredential", err)
	}
}

func TestOpen_BuildsEveryStoreAndTheHistory(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "u")
	t.Setenv(defaults.BasicPasswordEnv, "p")
	db, closeDB, err := open(filepath.Join(t.TempDir(), "weft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	a := adapter()
	for name, nilStore := range map[string]bool{
		"coordination": a.Coordination(db) == nil, "shards": a.Shards(db) == nil,
		"services": a.Services(db) == nil, "tokens": a.Tokens(db) == nil,
		"sources": a.Sources(db) == nil, "notices": a.Notices(db) == nil,
		"crons": a.Crons(db) == nil, "bins": a.Bins(db) == nil,
		"ingest failures": a.IngestFailures(db) == nil, "notifier": a.Notifier(db) == nil,
		"identity": a.Identity(db) == nil,
	} {
		if nilStore {
			t.Errorf("%s is nil", name)
		}
	}
	if db.history == nil {
		t.Error("no history repository")
	}
}
