package parquet_test

import (
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/archive/parquet"
)

func registry(t *testing.T) *archive.Registry {
	t.Helper()
	r := archive.NewRegistry()
	for _, f := range parquet.Formats() {
		if err := r.AddFormat(f); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestFormatOf_TellsAnOlderManifestByItsPath(t *testing.T) {
	const prefix = "s3://b/apps/shard1.attempt-1"
	r := registry(t)
	for _, c := range []struct {
		path, want string
	}{
		{prefix + "/sized/**/*.parquet", parquet.FormatSized},
		{prefix + "/k30/**/*.parquet", parquet.FormatKeyed},
		{prefix + "/m60/**/*.parquet", parquet.FormatTime},
		{prefix + "/**/*.parquet", parquet.FormatTime}, // before the width was recorded
		{"s3://b/apps/shard1.parquet", parquet.FormatFile},
	} {
		got, err := r.FormatOf(archive.Manifest{Path: c.path})
		if err != nil || got != c.want {
			t.Errorf("FormatOf(%q) = %q, %v; want %q", c.path, got, err, c.want)
		}
	}
}

func TestFormatOf_RefusesAPathOfNoKnownLayout(t *testing.T) {
	r := registry(t)
	for _, p := range []string{"", "s3://b/apps/shard1.csv", "s3://b/apps/*.parquet", "s3://b/apps/shard1"} {
		if id, err := r.FormatOf(archive.Manifest{Path: p}); err == nil {
			t.Errorf("FormatOf(%q) = %q, want an error", p, id)
		}
	}
}

// Two formats that recognized one path would make an old manifest ambiguous:
// every shape of path an archive can have is exactly one format's.
func TestFormats_NoPathIsTwoFormats(t *testing.T) {
	const prefix = "s3://b/apps/shard1.attempt-1"
	for _, path := range []string{
		prefix + ".parquet",
		prefix + "/m60/**/*.parquet",
		prefix + "/**/*.parquet",
		prefix + "/k30/**/*.parquet",
		prefix + "/sized/**/*.parquet",
		"s3://b/apps/x.csv",
		"",
	} {
		n := 0
		for _, f := range parquet.Formats() {
			if f.Recognizes(path) {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%q is recognized by %d formats", path, n)
		}
	}
}
