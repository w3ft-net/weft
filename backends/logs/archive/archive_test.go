package archive_test

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/shard"
)

type format string

func (f format) ID() string { return string(f) }

type archiver []string

func (a archiver) Writes() []string { return a }

type reader []string

func (r reader) Reads() []string { return r }

func TestFormatOf_TellsAnOlderManifestByItsPath(t *testing.T) {
	const prefix = "s3://b/apps/shard1.attempt-1"
	for _, c := range []struct {
		path, want string
	}{
		{prefix + "/sized/**/*.parquet", archive.FormatSized},
		{prefix + "/k30/**/*.parquet", archive.FormatKeyed},
		{prefix + "/m60/**/*.parquet", archive.FormatTime},
		{prefix + "/**/*.parquet", archive.FormatTime}, // before the width was recorded
		{"s3://b/apps/shard1.parquet", archive.FormatFile},
	} {
		got, err := archive.FormatOf(archive.Manifest{Path: c.path})
		if err != nil || got != c.want {
			t.Errorf("FormatOf(%q) = %q, %v; want %q", c.path, got, err, c.want)
		}
	}
}

func TestFormatOf_RecordedIDWinsOverPath(t *testing.T) {
	got, err := archive.FormatOf(archive.Manifest{FormatID: "x-v2", Path: "a/sized/**/*.parquet"})
	if err != nil || got != "x-v2" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFormatOf_RefusesAPathOfNoKnownLayout(t *testing.T) {
	for _, p := range []string{"", "s3://b/apps/shard1.csv", "s3://b/apps/*.parquet", "s3://b/apps/shard1"} {
		if id, err := archive.FormatOf(archive.Manifest{Path: p}); err == nil {
			t.Errorf("FormatOf(%q) = %q, want an error", p, id)
		}
	}
}

// The path rules are the ones the shard package writes and reads by; the two
// must not drift.
func TestFormatOf_AgreesWithTheShardPackage(t *testing.T) {
	for _, p := range []string{
		"s3://b/a.attempt-1/sized/**/*.parquet",
		"s3://b/a.attempt-1/k30/**/*.parquet",
		"s3://b/a.attempt-1/m30/**/*.parquet",
		"s3://b/a.attempt-1/**/*.parquet",
	} {
		id, err := archive.FormatOf(archive.Manifest{Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := id == archive.FormatSized, shard.IsSized(p); got != want {
			t.Errorf("%s: sized = %v, shard.IsSized = %v", p, got, want)
		}
		if got, want := id == archive.FormatSized || id == archive.FormatKeyed, shard.IsKeyed(p); got != want {
			t.Errorf("%s: keyed = %v, shard.IsKeyed = %v", p, got, want)
		}
	}
}

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
	r := newRegistry(t, []string{archive.FormatSized}, map[string]reader{"d": {archive.FormatSized}})
	rt, err := r.Route(archive.Config{Format: archive.FormatSized, Readers: map[string]string{archive.FormatSized: "d"}}, archiver{archive.FormatSized})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reader(archive.Manifest{Path: "s3://b/a.attempt-1/sized/**/*.parquet"}); err != nil {
		t.Fatalf("a manifest with no ID: %v", err)
	}
	// An archive in the older single-file layout has no reader configured.
	if _, err := rt.Reader(archive.Manifest{Path: "s3://b/a.parquet"}); err == nil {
		t.Fatal("a single-file archive found a reader")
	}
}

func TestRouter_CheckNamesEveryUnroutableArchive(t *testing.T) {
	r := newRegistry(t, []string{archive.FormatSized}, map[string]reader{"d": {archive.FormatSized}})
	rt, err := r.Route(archive.Config{Format: archive.FormatSized, Readers: map[string]string{archive.FormatSized: "d"}}, archiver{archive.FormatSized})
	if err != nil {
		t.Fatal(err)
	}
	ok := []archive.Manifest{{Path: "s3://b/a.attempt-1/sized/**/*.parquet"}, {FormatID: archive.FormatSized}}
	if err := rt.Check(ok); err != nil {
		t.Fatal(err)
	}
	err = rt.Check(append(ok, archive.Manifest{Path: "s3://b/old.parquet"}, archive.Manifest{Path: "s3://b/odd.csv"}))
	if err == nil || !strings.Contains(err.Error(), archive.FormatFile) || !strings.Contains(err.Error(), "odd.csv") {
		t.Fatalf("err = %v, want both the file-format archive and the unknown path named", err)
	}
}
