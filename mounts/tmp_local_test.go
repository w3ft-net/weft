package mounts

import (
	"context"
	"errors"
	"github.com/w3ft-net/weft/shape"
	"io/fs"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/tmp"
	"github.com/w3ft-net/weft/sqlitedb"
)

type countingNotices struct{ keys []string }

func (c *countingNotices) Maybe(_ context.Context, key string) { c.keys = append(c.keys, key) }

// /tmp runs over any tmp.Store: here the open build's SQLite one, with no
// Postgres and no user store behind the mount.
func TestTmpMount_OverTheLocalStore(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := defaults.NewTmpBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	notices := &countingNotices{}
	m := &TmpMount{cfg: Config{Tmp: st, UserID: "user-1", NoticeQueue: notices}}

	if err := m.Mkdir("tmp"); err != nil { // a directory called tmp is just a directory
		t.Fatal(err)
	}
	if err := m.WriteFile("tmp/note", []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendFile("tmp/note", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendFile("tmp/note", []byte("!\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReadFile("tmp/note"); err != nil || string(got) != "one\ntwo!\n" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	entries, err := m.ReadDir("tmp")
	if err != nil || len(entries) != 1 || entries[0].Name() != "note" {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	if info, err := entries[0].Info(); err != nil || info.Size() != 9 {
		t.Fatalf("size = %v, %v", info, err)
	}
	if err := m.Rename("tmp", "moved"); err != nil {
		t.Fatal(err)
	}
	if info, err := m.Stat("moved/note"); err != nil || info.IsDir() || info.Size() != 9 {
		t.Fatalf("after renaming the directory: %v, %v", info, err)
	}
	if _, err := m.Stat("tmp/note"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the old path remains: %v", err)
	}
	if err := m.CopyFile("moved/note", "copy"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveAll("moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadFile("moved/note"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("ReadFile of a removed file: %v", err)
	}
	// The first file a user made raised the notice once; later creations did not.
	if len(notices.keys) != 1 || notices.keys[0] != NoticeKeyTmpExpiry {
		t.Errorf("notices = %v, want one %s", notices.keys, NoticeKeyTmpExpiry)
	}

	// Limits are the store's: a cap of 4 bytes refuses a bigger write.
	small := &TmpMount{cfg: Config{Tmp: limitedTmp{st, tmp.Limits{MaxFileBytes: 4}}}}
	if err := small.WriteFile("big", []byte("12345")); !errors.Is(err, tmp.ErrFileTooLarge) {
		t.Errorf("a write over the store's limit: %v", err)
	}
}

// limitedTmp is a tmp.Store that holds every owner to lim.
type limitedTmp struct {
	tmp.Store
	lim tmp.Limits
}

func (l limitedTmp) Limits(shape.Added) tmp.Limits { return l.lim }
