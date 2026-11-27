package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("RATELIMIT_LOGIN_RPM: 77\nMIN_WEFTD_VERSION: \"0.40.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Load(context.Background(), FileSource{Path: path})
	if r.LoginRPM != 77 || r.MinWeftdVersion != "0.40.0" {
		t.Fatalf("got LoginRPM=%d MinWeftdVersion=%q", r.LoginRPM, r.MinWeftdVersion)
	}
}

func TestFileSource_MissingIsEmptyAndBadIsAnError(t *testing.T) {
	dir := t.TempDir()
	if m, err := (FileSource{Path: filepath.Join(dir, "none.yaml")}).Load(context.Background()); err != nil || len(m) != 0 {
		t.Fatalf("missing file: %v, %v", m, err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("a: [unclosed"), 0o644)
	if _, err := (FileSource{Path: bad}).Load(context.Background()); err == nil {
		t.Fatal("malformed YAML loaded without an error")
	}
}

func TestSourceFromEnv_EnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("RATELIMIT_LOGIN_RPM: 10\nRATELIMIT_LOGIN_BURST: 20\n"), 0o644)
	t.Setenv("WEFT_CONFIG_FILE", path)
	t.Setenv("RATELIMIT_LOGIN_RPM", "55")
	r := Load(context.Background(), SourceFromEnv(""))
	if r.LoginRPM != 55 || r.LoginBurst != 20 {
		t.Fatalf("got LoginRPM=%d LoginBurst=%d, want the env's 55 and the file's 20", r.LoginRPM, r.LoginBurst)
	}
}

func TestWatch_ReloadsAndKeepsValuesOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("RATELIMIT_LOGIN_RPM: 1\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ptr := Watch(ctx, FileSource{Path: path}, 10*time.Millisecond)
	if got := ptr.Load().LoginRPM; got != 1 {
		t.Fatalf("initial LoginRPM = %d", got)
	}
	os.WriteFile(path, []byte("RATELIMIT_LOGIN_RPM: 2\n"), 0o644)
	waitFor(t, func() bool { return ptr.Load().LoginRPM == 2 })
	os.WriteFile(path, []byte("a: [unclosed"), 0o644)
	time.Sleep(100 * time.Millisecond)
	if got := ptr.Load().LoginRPM; got != 2 {
		t.Fatalf("a failed reload changed LoginRPM to %d, want it kept at 2", got)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached")
}

func TestSourceFromEnv_NamedKinds(t *testing.T) {
	t.Setenv("WEFT_CONFIG_SOURCE", "fake")
	kind := Kind{Name: "fake", New: func(region string) Source { return fakeSource{"RATELIMIT_LOGIN_RPM": "42"} }}
	if r := Load(context.Background(), SourceFromEnv("", kind)); r.LoginRPM != 42 {
		t.Fatalf("LoginRPM = %d, want the kind's 42", r.LoginRPM)
	}
	// A name no kind answers to is an error on load, so the process falls back to
	// defaults loudly rather than reading nothing silently.
	if _, err := SourceFromEnv("").Load(context.Background()); err == nil {
		t.Fatal("an unknown WEFT_CONFIG_SOURCE loaded without an error")
	}
}

type fakeSource map[string]string

func (f fakeSource) Load(context.Context) (map[string]string, error) { return f, nil }
