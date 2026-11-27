package shell

import (
	"os"
	"path/filepath"
	"testing"
)

// withHome points HOME at a fresh temp dir for the duration of t.
// LoadConfig and SaveConfig both consult os.UserHomeDir(); without
// this isolation the tests would read/write the developer's real
// .weft.yaml.
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Some environments fall back to $USERPROFILE on Windows or
	// honor a separate cache. Both env vars are read by os.UserHomeDir.
	t.Setenv("USERPROFILE", dir)
	// Run from a clean cwd so .weft.yaml in CWD doesn't shadow the home file.
	cwd := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
	// Make sure env-var overrides from a previous test don't leak in.
	t.Setenv("WEFT_ENDPOINT", "")
	t.Setenv("WEFT_TOKEN", "")
	t.Setenv("WEFT_ORG", "")
	return dir
}

// TestLoadConfig_FromEnv: with no file present, env vars alone are
// enough to produce a valid config. Endpoint is mandatory; token and
// org carry through.
func TestLoadConfig_FromEnv(t *testing.T) {
	withHome(t)
	t.Setenv("WEFT_ENDPOINT", "https://api.example.test")
	t.Setenv("WEFT_TOKEN", "tok-abc")
	t.Setenv("WEFT_ORG", "acme")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Endpoint != "https://api.example.test" {
		t.Errorf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Token != "tok-abc" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.Org != "acme" {
		t.Errorf("Org = %q", cfg.Org)
	}
}

// TestLoadConfig_FromHomeFile: a .weft.yaml in HOME is picked up when
// no env vars are set. CWD is a separate temp dir so the file in HOME
// is the only candidate.
func TestLoadConfig_FromHomeFile(t *testing.T) {
	home := withHome(t)

	data := []byte("endpoint: https://api.home.test\ntoken: home-tok\norg: home-org\n")
	if err := os.WriteFile(filepath.Join(home, ".weft.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Endpoint != "https://api.home.test" {
		t.Errorf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Token != "home-tok" || cfg.Org != "home-org" {
		t.Errorf("got Token=%q Org=%q", cfg.Token, cfg.Org)
	}
}

// TestLoadConfig_CwdShadowsHome: a .weft.yaml in CWD beats the HOME
// copy. This is the contract LoadConfig advertises (CWD before HOME);
// pin it so a future refactor doesn't silently flip the precedence.
func TestLoadConfig_CwdShadowsHome(t *testing.T) {
	home := withHome(t)

	// HOME file with one set of values
	os.WriteFile(filepath.Join(home, ".weft.yaml"), []byte("endpoint: https://api.home.test\n"), 0o600)
	// CWD file with different ones
	cwd, _ := os.Getwd()
	os.WriteFile(filepath.Join(cwd, ".weft.yaml"), []byte("endpoint: https://api.cwd.test\n"), 0o600)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Endpoint != "https://api.cwd.test" {
		t.Errorf("Endpoint = %q, want CWD file's value", cfg.Endpoint)
	}
}

// TestLoadConfig_EnvOverridesFile: env wins over file values, but
// only for fields the env actually sets. WEFT_ENDPOINT overrides
// endpoint while file's token comes through unchanged.
func TestLoadConfig_EnvOverridesFile(t *testing.T) {
	home := withHome(t)
	os.WriteFile(filepath.Join(home, ".weft.yaml"),
		[]byte("endpoint: https://file.test\ntoken: file-tok\norg: file-org\n"), 0o600)

	t.Setenv("WEFT_ENDPOINT", "https://env.test")
	// Don't override token or org — those should come from the file.

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Endpoint != "https://env.test" {
		t.Errorf("Endpoint = %q, want env override", cfg.Endpoint)
	}
	if cfg.Token != "file-tok" || cfg.Org != "file-org" {
		t.Errorf("Token/Org should come from file: got Token=%q Org=%q", cfg.Token, cfg.Org)
	}
}

// TestLoadConfig_NoEndpoint: without endpoint configured, LoadConfig
// returns an actionable error. The CLI surfaces this to nudge the
// user to set the endpoint.
func TestLoadConfig_NoEndpoint(t *testing.T) {
	withHome(t)
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when no endpoint is configured")
	}
}

// TestSaveConfig_WritesToHome: SaveConfig drops .weft.yaml in HOME
// with 0600 perms (it contains a JWT). Round-trip through LoadConfig
// reads back the same values.
func TestSaveConfig_WritesToHome(t *testing.T) {
	home := withHome(t)

	in := &Config{
		Endpoint: "https://save.test",
		Token:    "save-tok",
		Org:      "save-org",
	}
	if err := SaveConfig(in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(filepath.Join(home, ".weft.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600 (file contains a token)", info.Mode().Perm())
	}

	out, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if *out != *in {
		t.Errorf("round-trip:\n  got:  %+v\n  want: %+v", *out, *in)
	}
}
