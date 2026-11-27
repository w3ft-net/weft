package sqlitedb

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOpenEnforcesForeignKeysAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weft.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE p (id INTEGER PRIMARY KEY); CREATE TABLE c (p INTEGER NOT NULL REFERENCES p (id) ON DELETE CASCADE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO c VALUES (1)`); err == nil {
		t.Fatal("a child without a parent was accepted: foreign keys are off")
	}
	if _, err := db.Exec(`INSERT INTO p VALUES (1); INSERT INTO c VALUES (1); DELETE FROM p`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM c`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("children after deleting the parent: %d err=%v", n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no database file: %v", err)
	}
}

// The open build links this package into processes that build with
// CGO_ENABLED=0; the driver is the only thing it imports that could need cgo.
func TestLoadsWithoutCgo(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go list -deps with CGO_ENABLED=0: %v\n%s", err, out)
	}
}
