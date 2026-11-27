package mounts

import (
	"context"
	"errors"
	"github.com/w3ft-net/weft/shape"
	"io/fs"
	"testing"

	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/backends/tmp"
)

type countingNotices struct{ keys []string }

func (c *countingNotices) Maybe(_ context.Context, key string) { c.keys = append(c.keys, key) }

// /tmp runs over any tmp.Store: here the open build's SQLite one, with no
// Postgres and no user store behind the mount.
func TestTmpMount_OverTheLocalStore(t *testing.T) {
	st := mem.NewTmp()
	notices := &countingNotices{}
	m := &TmpMount{cfg: Config{Tmp: st, UserID: "user-1", NoticeQueue: notices}}

	if err := m.Mkdir(context.Background(), "tmp"); err != nil { // a directory called tmp is just a directory
		t.Fatal(err)
	}
	if err := m.WriteFile(context.Background(), "tmp/note", []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendFile(context.Background(), "tmp/note", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendFile(context.Background(), "tmp/note", []byte("!\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReadFile(context.Background(), "tmp/note"); err != nil || string(got) != "one\ntwo!\n" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	entries, err := m.ReadDir(context.Background(), "tmp")
	if err != nil || len(entries) != 1 || entries[0].Name() != "note" {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	if info, err := entries[0].Info(); err != nil || info.Size() != 9 {
		t.Fatalf("size = %v, %v", info, err)
	}
	if err := m.Rename(context.Background(), "tmp", "moved"); err != nil {
		t.Fatal(err)
	}
	if info, err := m.Stat(context.Background(), "moved/note"); err != nil || info.IsDir() || info.Size() != 9 {
		t.Fatalf("after renaming the directory: %v, %v", info, err)
	}
	if _, err := m.Stat(context.Background(), "tmp/note"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the old path remains: %v", err)
	}
	if err := m.CopyFile(context.Background(), "moved/note", "copy"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveAll(context.Background(), "moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadFile(context.Background(), "moved/note"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("ReadFile of a removed file: %v", err)
	}
	// The first file a user made raised the notice once; later creations did not.
	if len(notices.keys) != 1 || notices.keys[0] != NoticeKeyTmpExpiry {
		t.Errorf("notices = %v, want one %s", notices.keys, NoticeKeyTmpExpiry)
	}

	// Limits are the store's: a cap of 4 bytes refuses a bigger write.
	small := &TmpMount{cfg: Config{Tmp: limitedTmp{st, tmp.Limits{MaxFileBytes: 4}}}}
	if err := small.WriteFile(context.Background(), "big", []byte("12345")); !errors.Is(err, tmp.ErrFileTooLarge) {
		t.Errorf("a write over the store's limit: %v", err)
	}
}

// limitedTmp is a tmp.Store that holds every owner to lim.
type limitedTmp struct {
	tmp.Store
	lim tmp.Limits
}

func (l limitedTmp) Limits(shape.Added) tmp.Limits { return l.lim }
