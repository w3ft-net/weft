//go:build !defaultshape

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
	m := &LogMount{cfg: ownerCfg(t, []string{
		"loom/prod/api/main",
		"loom/prod/api/metrics",
		"loom/prod/worker/main",
		"loom/edge/main", // env-less: <ns>/<svc>/<kind>
	})}

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
	m := &LogMount{cfg: ownerCfg(t, []string{"loom/prod/api/main", "loom/prod/api/metrics", "loom/prod/worker/main"})}
	got, err := m.Walk("loom/prod/api")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"loom/", "loom/prod/api/main.log", "loom/prod/api/metrics.log"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk(loom/prod/api) = %v, want %v", got, want)
	}
}

// The 3-segment form /log/<ns>/<svc>/<kind>.log reads a service across every
// env that has it (cat has always accepted it). ls, cd and stat must agree:
// /log/<ns>/<svc> is a directory of the kinds any env carries. It used to
// answer "file does not exist" for a path cat reads happily.
func TestLogMountThreeSegmentServiceSpansEnvs(t *testing.T) {
	m := &LogMount{cfg: ownerCfg(t, []string{
		"acme/prod/api/main",
		"acme/prod/api/metrics",
		"acme/sand/api/main",
		"acme/sand/api/heartbeats",
		"acme/sand/worker/main",
	})}
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

	if got, want := names("acme/api"), []string{"heartbeats.log", "main.log", "metrics.log"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDir(acme/api) = %v, want the union of kinds across envs %v", got, want)
	}
	if got, want := names("acme/worker"), []string{"main.log"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDir(acme/worker) = %v, want %v", got, want)
	}

	for rel, dir := range map[string]bool{
		"acme/api":               true,  // cd /log/acme/api
		"acme/api/main.log":      false, // a kind some env carries
		"acme/api/metrics.log":   false,
		"acme/worker/main.log":   false,
		"acme/prod/api/main.log": false, // 4-segment form unchanged
	} {
		fi, err := m.Stat(rel)
		if err != nil {
			t.Errorf("Stat(%q): %v", rel, err)
			continue
		}
		if fi.IsDir() != dir {
			t.Errorf("Stat(%q).IsDir() = %v, want %v", rel, fi.IsDir(), dir)
		}
	}
	for _, rel := range []string{"acme/nope", "acme/api/nope.log", "acme/worker/metrics.log", "other/api"} {
		if _, err := m.Stat(rel); err == nil {
			t.Errorf("Stat(%q) should not exist", rel)
		}
	}
	if _, err := m.ReadDir("acme/nope"); err == nil {
		t.Error("ReadDir of a service no env carries should fail")
	}

	// The namespace listing is unchanged: envs only, not duplicate virtual
	// service entries (those would double-list every service).
	if got, want := names("acme"), []string{"prod/", "sand/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDir(acme) = %v, want %v", got, want)
	}
}

// A real env-less service directory keeps precedence over the merged view.
func TestLogMountEnvlessServiceWinsOverMergedView(t *testing.T) {
	m := &LogMount{cfg: ownerCfg(t, []string{
		"acme/api/main",      // env-less: /log/acme/api is a real directory
		"acme/prod/api/main", // env-bearing service of the same name
		"acme/prod/api/metrics",
	})}
	ents, err := m.ReadDir("acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "main.log" {
		var got []string
		for _, e := range ents {
			got = append(got, e.Name())
		}
		t.Errorf("ReadDir(acme/api) = %v, want only the env-less service's main.log", got)
	}
}
