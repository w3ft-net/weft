// Package tmpstoretest is the conformance suite for tmp.Store: the behaviour
// every implementation of the /tmp domain (Postgres for the hosted build, SQLite
// for the open one) must show. An implementation's test calls Run with a
// constructor.
package tmpstoretest

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/tmp"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store tmp.Store
	// Owner owns the files the suite makes. The open build's is the empty value.
	Owner shape.Added
	// Other is a second owner, for a store that has owners to tell apart. Nil for
	// one that has none.
	Other *shape.Added
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"PathsAreNormalized", pathsAreNormalized},
		{"WriteAndRead", writeAndRead},
		{"WriteRules", writeRules},
		{"Mkdir", mkdir},
		{"List", list},
		{"NamesWithWildcards", namesWithWildcards},
		{"Append", appendFile},
		{"Touch", touch},
		{"Copy", copyFile},
		{"Rename", rename},
		{"RenameDirectory", renameDirectory},
		{"Remove", remove},
		{"RemoveAll", removeAll},
		{"SizeLimit", sizeLimit},
		{"Quota", quota},
		{"UsageAndCount", usageAndCount},
		{"OwnersAreSeparate", owners},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

var none = tmp.Limits{}

func write(t *testing.T, s Subject, path, data string) {
	t.Helper()
	if _, err := s.Store.Write(s.Owner, none, "u1", path, []byte(data)); err != nil {
		t.Fatalf("Write %q: %v", path, err)
	}
}

func mk(t *testing.T, s Subject, path string) {
	t.Helper()
	if err := s.Store.Mkdir(s.Owner, "u1", path); err != nil {
		t.Fatalf("Mkdir %q: %v", path, err)
	}
}

func read(t *testing.T, s Subject, path string) string {
	t.Helper()
	b, err := s.Store.Read(s.Owner, path)
	if err != nil {
		t.Fatalf("Read %q: %v", path, err)
	}
	return string(b)
}

func is(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

func paths(fs []tmp.File) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

func pathsAreNormalized(t *testing.T, s Subject) {
	write(t, s, "/a.txt/", "x")
	if got := read(t, s, "a.txt"); got != "x" {
		t.Fatalf("a path with slashes around it reads %q", got)
	}
	for _, bad := range []string{"", "/", "a//b", "a/./b", "../x", "a/..", "n\x00ul"} {
		_, err := s.Store.Write(s.Owner, none, "", bad, []byte("x"))
		is(t, "Write "+bad, err, tmp.ErrInvalidPath)
		_, err = s.Store.Stat(s.Owner, bad)
		is(t, "Stat "+bad, err, tmp.ErrInvalidPath)
	}
}

func writeAndRead(t *testing.T, s Subject) {
	// Every shape of content comes back byte for byte: the empty file, a final
	// newline or none, blank lines, text that is not ASCII, a carriage return.
	for _, data := range []string{"", "a", "a\n", "a\nb", "a\nb\n", "a\n\n", "\n", "\n\n", "héllo\nwörld\n", "a\r\nb\r\n", "no newline at all"} {
		write(t, s, "f", data)
		if got := read(t, s, "f"); got != data {
			t.Errorf("wrote %q, read %q", data, got)
		}
		if f, err := s.Store.Stat(s.Owner, "f"); err != nil || f.Size != int64(len(data)) {
			t.Errorf("wrote %q: Stat = %+v err=%v, want size %d", data, f, err, len(data))
		}
	}
	big := bytes.Repeat([]byte("0123456789abcdef\n"), 3000)
	if _, err := s.Store.Write(s.Owner, none, "", "big", big); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Store.Read(s.Owner, "big"); !bytes.Equal(got, big) {
		t.Errorf("a %d byte file read back as %d bytes", len(big), len(got))
	}
	if _, err := s.Store.Read(s.Owner, "absent"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("Read of an absent file: %v", err)
	}
}

func writeRules(t *testing.T, s Subject) {
	created, err := s.Store.Write(s.Owner, none, "u1", "f", []byte("one"))
	if err != nil || !created {
		t.Fatalf("first write: created=%v err=%v", created, err)
	}
	f, _ := s.Store.Stat(s.Owner, "f")
	if f.IsDir || f.Path != "f" || f.CreatedBy != "u1" || f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() || f.Name() != "f" {
		t.Fatalf("Stat: %+v", f)
	}
	time.Sleep(5 * time.Millisecond)
	created, err = s.Store.Write(s.Owner, none, "u2", "f", []byte("two"))
	if err != nil || created {
		t.Fatalf("an overwrite: created=%v err=%v", created, err)
	}
	g, _ := s.Store.Stat(s.Owner, "f")
	if g.CreatedBy != "u1" || !g.UpdatedAt.After(f.UpdatedAt) || !g.CreatedAt.Equal(f.CreatedAt) {
		t.Errorf("an overwrite keeps the creator and moves the update time: %+v then %+v", f, g)
	}
	if _, err := s.Store.Write(s.Owner, none, "", "nodir/f", []byte("x")); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("Write under a missing directory: %v, want ErrNotFound", err)
	}
	mk(t, s, "d")
	_, err = s.Store.Write(s.Owner, none, "", "d", []byte("x"))
	is(t, "Write over a directory", err, tmp.ErrIsDir)
	_, err = s.Store.Write(s.Owner, none, "", "f/child", []byte("x"))
	is(t, "Write under a file", err, tmp.ErrNotDir)
	if _, err := s.Store.Read(s.Owner, "d"); !errors.Is(err, tmp.ErrIsDir) {
		t.Errorf("Read of a directory: %v, want ErrIsDir", err)
	}
	// No creator is fine.
	if _, err := s.Store.Write(s.Owner, none, "", "anon", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Store.Stat(s.Owner, "anon"); a.CreatedBy != "" {
		t.Errorf("creator %q, want none", a.CreatedBy)
	}
}

func mkdir(t *testing.T, s Subject) {
	mk(t, s, "d")
	d, err := s.Store.Stat(s.Owner, "d")
	if err != nil || !d.IsDir || d.Size != 0 || d.CreatedBy != "u1" {
		t.Fatalf("Stat of a directory: %+v err=%v", d, err)
	}
	is(t, "Mkdir twice", s.Store.Mkdir(s.Owner, "", "d"), tmp.ErrExists)
	write(t, s, "f", "x")
	is(t, "Mkdir over a file", s.Store.Mkdir(s.Owner, "", "f"), tmp.ErrExists)
	is(t, "Mkdir under a missing directory", s.Store.Mkdir(s.Owner, "", "no/sub"), tmp.ErrNotFound)
	is(t, "Mkdir under a file", s.Store.Mkdir(s.Owner, "", "f/sub"), tmp.ErrNotDir)
	mk(t, s, "d/sub")
	if _, err := s.Store.Write(s.Owner, none, "", "d/sub/f", []byte("deep")); err != nil {
		t.Fatalf("a file two directories down: %v", err)
	}
	if _, err := s.Store.Stat(s.Owner, "absent"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("Stat of an absent path: %v", err)
	}
	// A directory may be called tmp: its name is not the /tmp it lives in.
	mk(t, s, "tmp")
	write(t, s, "tmp/f", "inside")
	if got := read(t, s, "tmp/f"); got != "inside" {
		t.Errorf("a file in a directory called tmp reads %q", got)
	}
	if got, _ := s.Store.List(s.Owner, "tmp"); !slices.Equal(paths(got), []string{"tmp/f"}) {
		t.Errorf("a directory called tmp lists %v", paths(got))
	}
}

func list(t *testing.T, s Subject) {
	if got, err := s.Store.List(s.Owner, ""); err != nil || len(got) != 0 {
		t.Fatalf("an empty top: %+v err=%v", got, err)
	}
	write(t, s, "b.txt", "bb")
	mk(t, s, "a")
	write(t, s, "a/one", "1")
	write(t, s, "a/two", "22")
	mk(t, s, "a/sub")
	write(t, s, "a/sub/deep", "x")

	top, err := s.Store.List(s.Owner, "")
	if err != nil || !slices.Equal(paths(top), []string{"a", "b.txt"}) {
		t.Fatalf("top: %v err=%v, want [a b.txt]: one level, by path", paths(top), err)
	}
	if !top[0].IsDir || top[1].IsDir || top[1].Size != 2 || top[1].CreatedBy != "u1" || top[1].UpdatedAt.IsZero() {
		t.Errorf("listed entries: %+v", top)
	}
	for _, dir := range []string{"/", "/a/", "a"} {
		got, err := s.Store.List(s.Owner, dir)
		want := []string{"a", "b.txt"}
		if dir != "/" {
			want = []string{"a/one", "a/sub", "a/two"}
		}
		if err != nil || !slices.Equal(paths(got), want) {
			t.Errorf("List(%q) = %v err=%v, want %v", dir, paths(got), err, want)
		}
	}
	if got, err := s.Store.List(s.Owner, "a/sub"); err != nil || !slices.Equal(paths(got), []string{"a/sub/deep"}) {
		t.Errorf("nested: %v err=%v", paths(got), err)
	}
	if got, _ := s.Store.List(s.Owner, "nowhere"); len(got) != 0 {
		t.Errorf("a missing directory lists %v", paths(got))
	}
}

// A name with _ or % is just a name: it neither matches its neighbours nor is
// matched by them.
func namesWithWildcards(t *testing.T, s Subject) {
	for _, d := range []string{"a_b", "axb", "100%", "100x"} {
		mk(t, s, d)
		write(t, s, d+"/f", d)
	}
	for _, d := range []string{"a_b", "axb", "100%", "100x"} {
		got, err := s.Store.List(s.Owner, d)
		if err != nil || !slices.Equal(paths(got), []string{d + "/f"}) {
			t.Errorf("List(%q) = %v err=%v, want only its own file", d, paths(got), err)
		}
	}
	if err := s.Store.RemoveAll(s.Owner, "a_b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.RemoveAll(s.Owner, "100%"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"axb/f", "100x/f"} {
		if _, err := s.Store.Stat(s.Owner, p); err != nil {
			t.Errorf("removing a neighbour removed %s: %v", p, err)
		}
	}
	// A sibling whose name starts with the removed one is not under it.
	mk(t, s, "x")
	mk(t, s, "xy")
	write(t, s, "xy/f", "1")
	if err := s.Store.RemoveAll(s.Owner, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Stat(s.Owner, "xy/f"); err != nil {
		t.Errorf("RemoveAll of x took xy/f: %v", err)
	}
}

func appendFile(t *testing.T, s Subject) {
	created, err := s.Store.Append(s.Owner, none, "u1", "log", []byte("one\n"))
	if err != nil || !created {
		t.Fatalf("append to an absent file: created=%v err=%v", created, err)
	}
	created, err = s.Store.Append(s.Owner, none, "u2", "log", []byte("two\n"))
	if err != nil || created {
		t.Fatalf("append to a file: created=%v err=%v", created, err)
	}
	if got := read(t, s, "log"); got != "one\ntwo\n" {
		t.Fatalf("after two appends: %q", got)
	}
	if f, _ := s.Store.Stat(s.Owner, "log"); f.Size != 8 || f.CreatedBy != "u1" {
		t.Errorf("Stat after appends: %+v", f)
	}
	// Onto the last byte, whether or not it ended a line.
	write(t, s, "raw", "a")
	for _, chunk := range []string{"b", "c\nd", "\n", "e\n\nf"} {
		if _, err := s.Store.Append(s.Owner, none, "", "raw", []byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, s, "raw"); got != "abc\nd\ne\n\nf" {
		t.Errorf("appending to an unterminated line: %q, want %q", got, "abc\nd\ne\n\nf")
	}
	if _, err := s.Store.Append(s.Owner, none, "", "raw", nil); err != nil {
		t.Fatalf("appending nothing: %v", err)
	}
	if got := read(t, s, "raw"); got != "abc\nd\ne\n\nf" {
		t.Errorf("appending nothing changed the file: %q", got)
	}
	mk(t, s, "d")
	_, err = s.Store.Append(s.Owner, none, "", "d", []byte("x"))
	is(t, "Append to a directory", err, tmp.ErrIsDir)
	_, err = s.Store.Append(s.Owner, none, "", "nodir/f", []byte("x"))
	is(t, "Append under a missing directory", err, tmp.ErrNotFound)
}

func touch(t *testing.T, s Subject) {
	created, err := s.Store.Touch(s.Owner, none, "u1", "f")
	if err != nil || !created {
		t.Fatalf("touching an absent file: created=%v err=%v", created, err)
	}
	f, _ := s.Store.Stat(s.Owner, "f")
	if f.IsDir || f.Size != 0 || f.CreatedBy != "u1" || read(t, s, "f") != "" {
		t.Fatalf("a touched file: %+v", f)
	}
	write(t, s, "f", "keep\n")
	before, _ := s.Store.Stat(s.Owner, "f")
	time.Sleep(5 * time.Millisecond)
	created, err = s.Store.Touch(s.Owner, none, "u2", "f")
	if err != nil || created {
		t.Fatalf("touching a file: created=%v err=%v", created, err)
	}
	after, _ := s.Store.Stat(s.Owner, "f")
	if !after.UpdatedAt.After(before.UpdatedAt) || after.Size != 5 || after.CreatedBy != "u1" || read(t, s, "f") != "keep\n" {
		t.Errorf("a touch moves the update time and nothing else: %+v then %+v", before, after)
	}
	mk(t, s, "d")
	d0, _ := s.Store.Stat(s.Owner, "d")
	time.Sleep(5 * time.Millisecond)
	if created, err := s.Store.Touch(s.Owner, none, "", "d"); err != nil || created {
		t.Fatalf("touching a directory: created=%v err=%v", created, err)
	}
	if d1, _ := s.Store.Stat(s.Owner, "d"); !d1.UpdatedAt.After(d0.UpdatedAt) || !d1.IsDir {
		t.Errorf("a touched directory: %+v then %+v", d0, d1)
	}
	_, err = s.Store.Touch(s.Owner, none, "", "nodir/f")
	is(t, "Touch under a missing directory", err, tmp.ErrNotFound)
}

func copyFile(t *testing.T, s Subject) {
	write(t, s, "src", "line one\nline two\n")
	mk(t, s, "d")
	if err := s.Store.Copy(s.Owner, none, "u2", "src", "d/dst"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, "d/dst"); got != "line one\nline two\n" {
		t.Errorf("the copy reads %q", got)
	}
	if f, _ := s.Store.Stat(s.Owner, "d/dst"); f.CreatedBy != "u2" || f.Size != 18 {
		t.Errorf("the copy: %+v, want it credited to its copier", f)
	}
	if got := read(t, s, "src"); got != "line one\nline two\n" {
		t.Errorf("the source changed: %q", got)
	}
	write(t, s, "d/dst", "other") // the copy is its own file
	if got := read(t, s, "src"); got != "line one\nline two\n" {
		t.Errorf("writing the copy changed the source: %q", got)
	}
	is(t, "Copy onto an existing path", s.Store.Copy(s.Owner, none, "", "src", "d/dst"), tmp.ErrExists)
	is(t, "Copy of a directory", s.Store.Copy(s.Owner, none, "", "d", "d2"), tmp.ErrIsDir)
	is(t, "Copy of an absent file", s.Store.Copy(s.Owner, none, "", "absent", "x"), tmp.ErrNotFound)
	is(t, "Copy into a missing directory", s.Store.Copy(s.Owner, none, "", "src", "no/x"), tmp.ErrNotFound)
	// The destination counts against the limits.
	is(t, "Copy over the size limit", s.Store.Copy(s.Owner, tmp.Limits{MaxFileBytes: 5}, "", "src", "small"), tmp.ErrFileTooLarge)
	is(t, "Copy over the quota", s.Store.Copy(s.Owner, tmp.Limits{QuotaBytes: 20}, "", "src", "more"), tmp.ErrQuotaExceeded)
}

func rename(t *testing.T, s Subject) {
	mk(t, s, "d")
	write(t, s, "f", "content\n")
	before, _ := s.Store.Stat(s.Owner, "f")
	time.Sleep(5 * time.Millisecond)
	if err := s.Store.Rename(s.Owner, "f", "d/g"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Stat(s.Owner, "f"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("the old path remains: %v", err)
	}
	g, err := s.Store.Stat(s.Owner, "d/g")
	if err != nil || g.CreatedBy != "u1" || g.Size != 8 || !g.UpdatedAt.After(before.UpdatedAt) || read(t, s, "d/g") != "content\n" {
		t.Fatalf("the renamed file: %+v err=%v", g, err)
	}
	write(t, s, "other", "x")
	is(t, "Rename onto an existing path", s.Store.Rename(s.Owner, "other", "d/g"), tmp.ErrExists)
	is(t, "Rename of an absent path", s.Store.Rename(s.Owner, "absent", "x"), tmp.ErrNotFound)
	is(t, "Rename into a missing directory", s.Store.Rename(s.Owner, "other", "no/x"), tmp.ErrNotFound)
	is(t, "Rename into a file", s.Store.Rename(s.Owner, "other", "d/g/x"), tmp.ErrNotDir)
	if got := read(t, s, "other"); got != "x" {
		t.Errorf("a refused rename moved the file: %q", got)
	}
}

func renameDirectory(t *testing.T, s Subject) {
	mk(t, s, "src")
	mk(t, s, "src/sub")
	write(t, s, "src/a", "A")
	write(t, s, "src/sub/b", "B")
	mk(t, s, "srcx") // a sibling that shares a prefix
	write(t, s, "srcx/c", "C")
	mk(t, s, "dest")

	if err := s.Store.Rename(s.Owner, "src", "dest/moved"); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"dest/moved/a": "A", "dest/moved/sub/b": "B", "srcx/c": "C"} {
		if got := read(t, s, p); got != want {
			t.Errorf("%s reads %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{"src", "src/a", "src/sub", "src/sub/b"} {
		if _, err := s.Store.Stat(s.Owner, p); !errors.Is(err, tmp.ErrNotFound) {
			t.Errorf("%s remains after the move: %v", p, err)
		}
	}
	if got, _ := s.Store.List(s.Owner, "dest/moved"); !slices.Equal(paths(got), []string{"dest/moved/a", "dest/moved/sub"}) {
		t.Errorf("the moved directory lists %v", paths(got))
	}
	is(t, "Rename a directory into itself", s.Store.Rename(s.Owner, "dest", "dest/moved/inside"), tmp.ErrInvalidPath)
}

func remove(t *testing.T, s Subject) {
	write(t, s, "f", "x")
	if err := s.Store.Remove(s.Owner, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Stat(s.Owner, "f"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("the removed file remains: %v", err)
	}
	is(t, "Remove of an absent path", s.Store.Remove(s.Owner, "f"), tmp.ErrNotFound)
	mk(t, s, "d")
	write(t, s, "d/child", "x")
	is(t, "Remove of a directory with a child", s.Store.Remove(s.Owner, "d"), tmp.ErrNotEmpty)
	if err := s.Store.Remove(s.Owner, "d/child"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Remove(s.Owner, "d"); err != nil {
		t.Fatalf("Remove of an empty directory: %v", err)
	}
	// The name is free again, and the usage went with the file.
	write(t, s, "f", "again")
	if n, _ := s.Store.UsageBytes(s.Owner); n != 5 {
		t.Errorf("usage %d, want 5", n)
	}
}

func removeAll(t *testing.T, s Subject) {
	mk(t, s, "d")
	mk(t, s, "d/sub")
	write(t, s, "d/a", "1")
	write(t, s, "d/sub/b", "22")
	write(t, s, "keep", "333")
	if err := s.Store.RemoveAll(s.Owner, "d"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"d", "d/a", "d/sub", "d/sub/b"} {
		if _, err := s.Store.Stat(s.Owner, p); !errors.Is(err, tmp.ErrNotFound) {
			t.Errorf("%s remains: %v", p, err)
		}
	}
	if got := read(t, s, "keep"); got != "333" {
		t.Errorf("an unrelated file: %q", got)
	}
	if n, _ := s.Store.UsageBytes(s.Owner); n != 3 {
		t.Errorf("usage %d, want 3", n)
	}
	is(t, "RemoveAll of an absent path", s.Store.RemoveAll(s.Owner, "d"), tmp.ErrNotFound)
	// A file is removable the same way.
	if err := s.Store.RemoveAll(s.Owner, "keep"); err != nil {
		t.Fatal(err)
	}
}

func sizeLimit(t *testing.T, s Subject) {
	lim := tmp.Limits{MaxFileBytes: 10}
	if _, err := s.Store.Write(s.Owner, lim, "", "ok", []byte("0123456789")); err != nil {
		t.Fatalf("a file at the limit: %v", err)
	}
	_, err := s.Store.Write(s.Owner, lim, "", "big", []byte("0123456789x"))
	is(t, "a file over the limit", err, tmp.ErrFileTooLarge)
	if _, err := s.Store.Stat(s.Owner, "big"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("a refused write left a file: %v", err)
	}
	// Appending counts the whole file, not the chunk.
	if _, err := s.Store.Write(s.Owner, lim, "", "grow", []byte("012345")); err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.Append(s.Owner, lim, "", "grow", []byte("67890"))
	is(t, "an append past the limit", err, tmp.ErrFileTooLarge)
	if got := read(t, s, "grow"); got != "012345" {
		t.Errorf("a refused append changed the file: %q", got)
	}
	if _, err := s.Store.Append(s.Owner, lim, "", "grow", []byte("6789")); err != nil {
		t.Fatalf("an append up to the limit: %v", err)
	}
	// Zero is no limit.
	if _, err := s.Store.Write(s.Owner, none, "", "huge", bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Fatalf("with no limit: %v", err)
	}
}

func quota(t *testing.T, s Subject) {
	lim := tmp.Limits{QuotaBytes: 20}
	if _, err := s.Store.Write(s.Owner, lim, "", "a", bytes.Repeat([]byte("a"), 12)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Store.Write(s.Owner, lim, "", "b", bytes.Repeat([]byte("b"), 9))
	is(t, "a file past the quota", err, tmp.ErrQuotaExceeded)
	if _, err := s.Store.Stat(s.Owner, "b"); !errors.Is(err, tmp.ErrNotFound) {
		t.Errorf("a refused write left a file: %v", err)
	}
	if _, err := s.Store.Write(s.Owner, lim, "", "b", bytes.Repeat([]byte("b"), 8)); err != nil {
		t.Fatalf("a file that fills the quota: %v", err)
	}
	// A rewrite counts only its growth: the same size, or a smaller one, is fine at
	// the quota, and a bigger one is not.
	if _, err := s.Store.Write(s.Owner, lim, "", "a", bytes.Repeat([]byte("A"), 12)); err != nil {
		t.Errorf("rewriting at the quota: %v", err)
	}
	if _, err := s.Store.Write(s.Owner, lim, "", "a", bytes.Repeat([]byte("A"), 4)); err != nil {
		t.Errorf("shrinking at the quota: %v", err)
	}
	_, err = s.Store.Write(s.Owner, lim, "", "b", bytes.Repeat([]byte("B"), 17))
	is(t, "growing past the quota", err, tmp.ErrQuotaExceeded)
	// Appends count what they add; touches and directories are free.
	_, err = s.Store.Append(s.Owner, lim, "", "b", bytes.Repeat([]byte("x"), 9))
	is(t, "an append past the quota", err, tmp.ErrQuotaExceeded)
	if _, err := s.Store.Append(s.Owner, lim, "", "b", bytes.Repeat([]byte("x"), 8)); err != nil {
		t.Errorf("an append up to the quota: %v", err)
	}
	mk(t, s, "d")
	if _, err := s.Store.Touch(s.Owner, lim, "", "empty"); err != nil {
		t.Errorf("an empty file at the quota: %v", err)
	}
}

func usageAndCount(t *testing.T, s Subject) {
	if n, err := s.Store.UsageBytes(s.Owner); err != nil || n != 0 {
		t.Fatalf("empty usage: %d err=%v", n, err)
	}
	mk(t, s, "d")
	write(t, s, "d/a", "12345")
	if _, err := s.Store.Write(s.Owner, none, "u2", "b", []byte("123")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Write(s.Owner, none, "u1", "c", nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Store.UsageBytes(s.Owner); n != 8 {
		t.Errorf("usage %d, want 8: files only, directories are free", n)
	}
	for by, want := range map[string]int64{"u1": 2, "u2": 1, "nobody": 0} {
		if n, err := s.Store.CountByUser(s.Owner, by); err != nil || n != want {
			t.Errorf("CountByUser(%q) = %d err=%v, want %d: files only", by, n, err, want)
		}
	}
}

func owners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("the store has no owners to tell apart")
	}
	other := *s.Other
	write(t, s, "f", "mine")
	if _, err := s.Store.Stat(other, "f"); !errors.Is(err, tmp.ErrNotFound) {
		t.Fatalf("another owner sees my file: %v", err)
	}
	if _, err := s.Store.Write(other, none, "x", "f", []byte("theirs")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, "f"); got != "mine" {
		t.Errorf("my file reads %q after the other owner wrote the same path", got)
	}
	if b, _ := s.Store.Read(other, "f"); string(b) != "theirs" {
		t.Errorf("their file reads %q", b)
	}
	if n, _ := s.Store.UsageBytes(other); n != 6 {
		t.Errorf("their usage %d, want 6", n)
	}
	if n, _ := s.Store.CountByUser(other, "u1"); n != 0 {
		t.Errorf("their count includes my files: %d", n)
	}
	if err := s.Store.RemoveAll(other, "f"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, "f"); got != "mine" {
		t.Errorf("the other owner's remove took my file: %q", got)
	}
	// Their quota is theirs.
	lim := tmp.Limits{QuotaBytes: 10}
	if _, err := s.Store.Write(other, lim, "", "g", bytes.Repeat([]byte("g"), 10)); err != nil {
		t.Errorf("my usage counted against theirs: %v", err)
	}
	if got, _ := s.Store.List(other, ""); !slices.Equal(paths(got), []string{"g"}) {
		t.Errorf("they list %v", paths(got))
	}
}
