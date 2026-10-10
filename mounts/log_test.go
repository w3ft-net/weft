package mounts

import (
	"io/fs"
	"reflect"
	"testing"
)

// Real LogServices entries carry the stream kind as their last segment
// ("<ns>/<env>/<svc>/<kind>", or "<ns>/<svc>/<kind>" when env-less), so a
// service is a directory holding <kind>.log files. cd already accepts
// /log/loom/prod/api; ls must list it too.
func TestLogMountServiceIsDirectory(t *testing.T) {
	m := &LogMount{cfg: Config{
		IsOwner: true,
		LogServices: []string{
			"loom/prod/api/main",
			"loom/prod/api/metrics",
			"loom/prod/worker/main",
			"loom/edge/main", // env-less: <ns>/<svc>/<kind>
		},
	}}

	names := func(rel string) []string {
		t.Helper()
		ents, err := m.ReadDir(rel)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", rel, err)
		}
		var out []string
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() {
				n += "/"
			}
			out = append(out, n)
		}
		return out
	}

	cases := []struct {
		rel  string
		want []string
	}{
		{"loom", []string{"prod/", "edge/"}},
		{"loom/prod", []string{"api/", "worker/"}},
		{"loom/prod/api", []string{"main.log", "metrics.log"}},
		{"loom/edge", []string{"main.log"}},
	}
	for _, c := range cases {
		if got := names(c.rel); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ReadDir(%q) = %v, want %v", c.rel, got, c.want)
		}
	}

	// Stat agrees with ReadDir.
	for rel, wantDir := range map[string]bool{
		"loom/prod/api":          true,
		"loom/prod/api/main.log": false,
		"loom/edge":              true,
	} {
		fi, err := m.Stat(rel)
		if err != nil {
			t.Errorf("Stat(%q): %v", rel, err)
			continue
		}
		if fi.IsDir() != wantDir {
			t.Errorf("Stat(%q).IsDir() = %v, want %v", rel, fi.IsDir(), wantDir)
		}
	}
	if _, err := m.Stat("loom/prod/nope"); err != fs.ErrNotExist {
		t.Errorf("Stat(loom/prod/nope) err = %v, want ErrNotExist", err)
	}
}

// Walk of a service directory returns only that service's files.
func TestLogMountWalkServiceSubtree(t *testing.T) {
	m := &LogMount{cfg: Config{
		IsOwner:     true,
		LogServices: []string{"loom/prod/api/main", "loom/prod/api/metrics", "loom/prod/worker/main"},
	}}
	got, err := m.Walk("loom/prod/api")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"loom/", "loom/prod/api/main.log", "loom/prod/api/metrics.log"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk(loom/prod/api) = %v, want %v", got, want)
	}
}
