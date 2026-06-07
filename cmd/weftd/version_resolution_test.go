package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/weftd"
)

// withFreshVersionState clears the package-level version maps for the
// duration of the test and restores them afterwards. The maps are
// global because they're populated once during weftd startup and read
// on the row-stamping hot path; tests need to swap them per-case.
func withFreshVersionState(t *testing.T) {
	t.Helper()
	prevReaders := versionReaders
	prevEnvs := versionEnvs
	versionReaders = map[string]*weftd.VersionReader{}
	versionEnvs = map[string]string{}
	t.Cleanup(func() {
		versionReaders = prevReaders
		versionEnvs = prevEnvs
	})
}

// TestVersionForApp_ResolutionOrder asserts the precedence
// versionForService documents: per-source `version.file` (live) wins
// over per-source `version.env` (snapshot at startup), which wins
// over the host-wide `dimensions.version` fallback. Without this
// ordering the rebuild story for env-var deploys silently shadows
// any later switch to file-based versioning.
func TestVersionForApp_ResolutionOrder(t *testing.T) {
	withFreshVersionState(t)

	dir := t.TempDir()
	vfile := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(vfile, []byte("v1.2.3-from-file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Seed three apps representing each branch.
	versionReaders["myco/file-app"] = weftd.NewVersionReader(context.Background(), vfile)
	versionEnvs["myco/env-app"] = "v9.9.9-from-env"
	// "myco/fallback-app" gets neither — should hit the
	// dimensions.version fallback. With no dimensionReaders entry
	// for "version" it ends up "".

	// Allow the file watcher's initial read to land. NewVersionReader
	// reads synchronously on construction, but assert via Current()
	// rather than racing on internals.
	if got := versionReaders["myco/file-app"].Current(); got != "v1.2.3-from-file" {
		t.Fatalf("file reader Current() = %q, want v1.2.3-from-file", got)
	}

	cases := []struct {
		app  string
		want string
	}{
		{"myco/file-app", "v1.2.3-from-file"},
		{"myco/env-app", "v9.9.9-from-env"},
		{"myco/fallback-app", ""}, // no file, no env, no dimensions.version
		{"myco/unconfigured-app", ""},
	}
	for _, tc := range cases {
		if got := versionForService(tc.app); got != tc.want {
			t.Errorf("versionForService(%q) = %q, want %q", tc.app, got, tc.want)
		}
	}
}

// TestVersionForApp_FileBeatsEnv covers the "both configured" case.
// The struct comment promises file wins; this codifies it so a refactor
// can't quietly invert the precedence.
func TestVersionForApp_FileBeatsEnv(t *testing.T) {
	withFreshVersionState(t)

	dir := t.TempDir()
	vfile := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(vfile, []byte("from-file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	versionReaders["myco/api"] = weftd.NewVersionReader(context.Background(), vfile)
	versionEnvs["myco/api"] = "from-env"

	if got := versionForService("myco/api"); got != "from-file" {
		t.Errorf("versionForService = %q, want from-file (file must beat env)", got)
	}
}

// TestVersionForApp_EnvCapturedEmptyStaysEmpty: when `version.env:`
// is configured but the env var is unset at agent startup, weftd
// captures "". Subsequent `versionForService` calls must return that
// captured "" — NOT fall through to dimensions.version. This is the
// behaviour that lets an operator "explicitly disable" the dimension
// for a source.
func TestVersionForApp_EnvCapturedEmptyStaysEmpty(t *testing.T) {
	withFreshVersionState(t)

	// Configured (key present) but the env was empty at startup.
	versionEnvs["myco/api"] = ""

	if got := versionForService("myco/api"); got != "" {
		t.Errorf("versionForService = %q, want empty (configured-but-unset env should not fall through)", got)
	}
}

// TestVersionConfig_YAMLNested asserts the new nested-key shape
// parses correctly. Before this commit the config used flat
// `version_file:`; after, callers write `version: { file:, env: }`.
// Round-trip through yaml.Unmarshal so a stray rename of the YAML
// tag turns this red.
func TestVersionConfig_YAMLNested(t *testing.T) {
	src := []byte(`
sources:
  - app: api
    path: /var/log/api.log
    version:
      file: /opt/api/VERSION
      env: APP_VERSION
  - app: web
    path: /var/log/web.log
    version:
      env: WEB_VERSION
  - app: legacy
    path: /var/log/legacy.log
`)

	var cfg Config
	if err := yaml.Unmarshal(src, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.Sources) != 3 {
		t.Fatalf("len(Sources) = %d, want 3", len(cfg.Sources))
	}
	if got, want := cfg.Sources[0].Version.File, "/opt/api/VERSION"; got != want {
		t.Errorf("Sources[0].Version.File = %q, want %q", got, want)
	}
	if got, want := cfg.Sources[0].Version.Env, "APP_VERSION"; got != want {
		t.Errorf("Sources[0].Version.Env = %q, want %q", got, want)
	}
	if got, want := cfg.Sources[1].Version.Env, "WEB_VERSION"; got != want {
		t.Errorf("Sources[1].Version.Env = %q, want %q", got, want)
	}
	if cfg.Sources[1].Version.File != "" {
		t.Errorf("Sources[1].Version.File = %q, want empty", cfg.Sources[1].Version.File)
	}
	// Source[2] declares no version block; both subkeys should be empty.
	if cfg.Sources[2].Version.File != "" || cfg.Sources[2].Version.Env != "" {
		t.Errorf("Sources[2].Version = %+v, want zero value", cfg.Sources[2].Version)
	}
}

// TestVersionConfig_YAMLFlatKeyRejected: the old flat `version_file:`
// schema is gone. A config still using it should round-trip with the
// new field empty — the rename is intentional and breaking, no
// alias. This codifies the no-legacy-support stance.
func TestVersionConfig_YAMLFlatKeyRejected(t *testing.T) {
	src := []byte(`
sources:
  - app: api
    path: /var/log/api.log
    version_file: /opt/api/VERSION
`)

	var cfg Config
	if err := yaml.Unmarshal(src, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Sources[0].Version.File != "" {
		t.Errorf("Sources[0].Version.File = %q, want empty (flat version_file: is not aliased)", cfg.Sources[0].Version.File)
	}
}
