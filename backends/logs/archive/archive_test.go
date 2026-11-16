package archive_test

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// format is a format whose archives' paths end in its own ID.
type format string

func (f format) ID() string                  { return string(f) }
func (f format) Recognizes(path string) bool { return strings.HasSuffix(path, "."+string(f)) }

type archiver []string

func (a archiver) Writes() []string { return a }

// reader is a reader of the formats it lists, over DuckDB's dialect.
type reader []string

func (r reader) Reads() []string                              { return r }
func (r reader) Dialect() dialect.Dialect                     { return dialect.DuckDB }
func (r reader) Open(archive.Archive) (archive.Handle, error) { return nil, nil }

func newRegistry(t *testing.T, formats []string, readers map[string]reader) *archive.Registry {
	t.Helper()
	r := archive.NewRegistry()
	for _, f := range formats {
		if err := r.AddFormat(format(f)); err != nil {
			t.Fatal(err)
		}
	}
	for n, rd := range readers {
		if err := r.AddReader(n, rd); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestFormatOf_ARecordedIDWinsOverThePath(t *testing.T) {
	r := newRegistry(t, []string{"a"}, nil)
	got, err := r.FormatOf(archive.Manifest{FormatID: "x-v2", Path: "s3://b/arch.a"})
	if err != nil || got != "x-v2" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// An older manifest says what it is by its path: the one registered format that
// recognizes it.
func TestFormatOf_TellsAnOlderManifestByItsPath(t *testing.T) {
	r := newRegistry(t, []string{"a", "b"}, nil)
	for path, want := range map[string]string{"s3://x/one.a": "a", "s3://x/two.b": "b"} {
		got, err := r.FormatOf(archive.Manifest{Path: path})
		if err != nil || got != want {
			t.Errorf("FormatOf(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestFormatOf_RefusesAPathNoFormatOrSeveralFormatsRecognize(t *testing.T) {
	r := archive.NewRegistry()
	for _, f := range []archive.Format{format("a"), format("b"), overlapping{"c", "b"}} {
		if err := r.AddFormat(f); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := r.FormatOf(archive.Manifest{Path: "s3://x/none.zzz"}); err == nil || !strings.Contains(err.Error(), "no registered format") {
		t.Errorf("a path nothing recognizes = %q, %v", id, err)
	}
	if id, err := r.FormatOf(archive.Manifest{Path: "s3://x/shared.b"}); err == nil || !strings.Contains(err.Error(), "all recognize") {
		t.Errorf("a path two formats recognize = %q, %v", id, err)
	}
}

// overlapping is a format that recognizes another's paths too, as two formats
// must never do.
type overlapping struct{ id, also string }

func (o overlapping) ID() string { return o.id }
func (o overlapping) Recognizes(path string) bool {
	return strings.HasSuffix(path, "."+o.id) || strings.HasSuffix(path, "."+o.also)
}

func TestRegistry_RefusesBadRegistrations(t *testing.T) {
	r := archive.NewRegistry()
	for _, id := range []string{"", archive.Default} {
		if r.AddFormat(format(id)) == nil {
			t.Errorf("AddFormat(%q) succeeded", id)
		}
	}
	if err := r.AddFormat(format("a")); err != nil {
		t.Fatal(err)
	}
	if r.AddFormat(format("a")) == nil {
		t.Error("a format registered twice")
	}
	if r.AddReader("", reader{}) == nil {
		t.Error("a reader with no name")
	}
	if err := r.AddReader("r", reader{"a"}); err != nil {
		t.Fatal(err)
	}
	if r.AddReader("r", reader{"a"}) == nil {
		t.Error("a reader registered twice")
	}
}

func TestRoute_Validates(t *testing.T) {
	formats := []string{"a", "b"}
	readers := map[string]reader{"ra": {"a"}, "rab": {"a", "b"}}
	for _, c := range []struct {
		name string
		cfg  archive.Config
		arch archiver
		want string // substring of the error; "" for none
	}{
		{"one format, one reader", archive.Config{Format: "a", Readers: map[string]string{"a": "ra"}}, archiver{"a"}, ""},
		{"a default reader serves the configured format", archive.Config{Format: "b", Readers: map[string]string{"default": "rab"}}, archiver{"b"}, ""},
		{"unregistered format", archive.Config{Format: "zzz", Readers: map[string]string{"default": "rab"}}, archiver{"zzz"}, "not registered"},
		{"archiver does not write it", archive.Config{Format: "b", Readers: map[string]string{"b": "rab"}}, archiver{"a"}, "does not write"},
		{"no reader for the configured format", archive.Config{Format: "b", Readers: map[string]string{"a": "ra"}}, archiver{"b"}, "no reader is configured"},
		{"unregistered reader", archive.Config{Format: "a", Readers: map[string]string{"a": "nope"}}, archiver{"a"}, "not registered"},
		{"reader does not read the entry's format", archive.Config{Format: "a", Readers: map[string]string{"a": "ra", "b": "ra"}}, archiver{"a"}, "does not read format \"b\""},
		{"default reader that cannot read the configured format", archive.Config{Format: "b", Readers: map[string]string{"default": "ra"}}, archiver{"b"}, "default reader"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := newRegistry(t, formats, readers).Route(c.cfg, c.arch)
			switch {
			case c.want == "" && err != nil:
				t.Fatal(err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestRouter_ExplicitEntryWinsOverTheDefault(t *testing.T) {
	ra, rab := reader{"a"}, reader{"a", "b"}
	r := newRegistry(t, []string{"a", "b"}, map[string]reader{"ra": ra, "rab": rab})
	rt, err := r.Route(archive.Config{Format: "a", Readers: map[string]string{"a": "ra", "default": "rab"}}, archiver{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := rt.Reader(archive.Manifest{FormatID: "a"}); len(got.Reads()) != 1 {
		t.Errorf("format a went to %v, want the explicit reader", got.Reads())
	}
	if got, _ := rt.Reader(archive.Manifest{FormatID: "b"}); len(got.Reads()) != 2 {
		t.Errorf("format b went to %v, want the default", got.Reads())
	}
}

// A default reader is used only for what it reads: the query fails rather than
// the reader guessing at a layout it does not know.
func TestRouter_DefaultNeverGuesses(t *testing.T) {
	r := newRegistry(t, []string{"a", "b"}, map[string]reader{"ra": {"a"}})
	rt, err := r.Route(archive.Config{Format: "a", Readers: map[string]string{"default": "ra"}}, archiver{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reader(archive.Manifest{FormatID: "b"}); err == nil {
		t.Fatal("the default reader was used for a format it does not read")
	}
	if _, err := rt.Reader(archive.Manifest{FormatID: "unknown"}); err == nil {
		t.Fatal("an unknown format found a reader")
	}
}

func TestRouter_ReaderResolvesAnOlderManifestThroughTheSameTable(t *testing.T) {
	r := newRegistry(t, []string{"a", "b"}, map[string]reader{"d": {"a"}})
	rt, err := r.Route(archive.Config{Format: "a", Readers: map[string]string{"a": "d"}}, archiver{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reader(archive.Manifest{Path: "s3://b/arch.a"}); err != nil {
		t.Fatalf("a manifest with no ID: %v", err)
	}
	// An archive in another format has no reader configured.
	if _, err := rt.Reader(archive.Manifest{Path: "s3://b/arch.b"}); err == nil {
		t.Fatal("an archive in a format with no reader found one")
	}
}

func TestRouter_CheckNamesEveryUnroutableArchive(t *testing.T) {
	r := newRegistry(t, []string{"a", "b"}, map[string]reader{"d": {"a"}})
	rt, err := r.Route(archive.Config{Format: "a", Readers: map[string]string{"a": "d"}}, archiver{"a"})
	if err != nil {
		t.Fatal(err)
	}
	ok := []archive.Manifest{{Path: "s3://b/arch.a"}, {FormatID: "a"}}
	if err := rt.Check(ok); err != nil {
		t.Fatal(err)
	}
	err = rt.Check(append(ok, archive.Manifest{Path: "s3://b/old.b"}, archive.Manifest{Path: "s3://b/odd.csv"}))
	if err == nil || !strings.Contains(err.Error(), `"b"`) || !strings.Contains(err.Error(), "odd.csv") {
		t.Fatalf("err = %v, want both the archive in the unread format and the unknown path named", err)
	}
}

// A process that only reads archives builds its Router from the reader entries
// alone: there is no archiver to check, but there must be a reader.
func TestReaders_NeedsAReaderAndRegisteredNames(t *testing.T) {
	r := newRegistry(t, []string{"a"}, map[string]reader{"ra": {"a"}, "rb": {"b"}})
	for name, c := range map[string]struct {
		readers map[string]string
		want    string // substring of the error; "" for none
	}{
		"one reader":                   {map[string]string{"a": "ra"}, ""},
		"a default alone":              {map[string]string{"default": "ra"}, ""},
		"none":                         {nil, "no reader is configured"},
		"an empty map":                 {map[string]string{}, "no reader is configured"},
		"a format nobody registered":   {map[string]string{"b": "rb"}, "format that is not registered"},
		"a reader nobody registered":   {map[string]string{"a": "nope"}, "not registered"},
		"a reader that cannot read it": {map[string]string{"a": "rb"}, "does not read format"},
	} {
		t.Run(name, func(t *testing.T) {
			rt, err := r.Readers(c.readers)
			switch {
			case c.want == "" && (err != nil || rt == nil):
				t.Fatalf("rt = %v, err = %v", rt, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}
