package cluster

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

const file = `
region: eu-west-1
base_url: https://weft.example.com
redis:
  addr: cache:6379
  tls: true
coordinator:
  host: coord.internal
  local_port: 9125
  internal_port: 9126
compute:
  host: compute.internal
  port: 9110
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "weft.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// One fact is written once and every server that dials or listens on it gets it.
func TestFlags_EveryServerGetsTheFactsItUses(t *testing.T) {
	c, err := Load(write(t, file))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		API:         {"coordinator": "localhost:9125", "base-url": "https://weft.example.com", "redis-addr": "cache:6379", "redis-tls": "true"},
		Coordinator: {"local-addr": "127.0.0.1:9125", "internal-addr": ":9126", "compute": "compute.internal:9110", "region": "eu-west-1", "share-url-base": "https://weft.example.com", "redis-addr": "cache:6379", "redis-tls": "true"},
		Compute:     {"port": "9110"},
		Shard:       {"coordinator-addr": "coord.internal:9126"},
	}
	for server, w := range want {
		got := c.Flags(server)
		if len(got) != len(w) {
			t.Errorf("%s: flags = %v, want %v", server, got, w)
		}
		for k, v := range w {
			if got[k] != v {
				t.Errorf("%s: --%s = %q, want %q", server, k, got[k], v)
			}
		}
	}
}

// The api's dial and the coordinator's listen come from the same port, so they
// cannot drift.
func TestFlags_DialAndListenShareOnePort(t *testing.T) {
	c, err := Load(write(t, file))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Flags(API)["coordinator"], "localhost:9125"; got != want {
		t.Errorf("api dials %q, want %q", got, want)
	}
	if got, want := c.Flags(Coordinator)["local-addr"], "127.0.0.1:9125"; got != want {
		t.Errorf("coordinator listens on %q, want %q", got, want)
	}
}

func apiFlags() (*flag.FlagSet, *string, *string, *bool) {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	coord := fs.String("coordinator", "localhost:9025", "")
	base := fs.String("base-url", "https://w3ft.net", "")
	tls := fs.Bool("redis-tls", false, "")
	fs.String("redis-addr", "", "")
	return fs, coord, base, tls
}

// A flag given on the command line wins over the file, and one it left alone
// takes the file's value.
func TestOverlay_AFlagGivenWinsOverTheFile(t *testing.T) {
	fs, coord, base, tls := apiFlags()
	if err := fs.Parse([]string{"-coordinator", "elsewhere:1"}); err != nil {
		t.Fatal(err)
	}
	if err := Overlay(fs, API, write(t, file)); err != nil {
		t.Fatal(err)
	}
	if *coord != "elsewhere:1" {
		t.Errorf("--coordinator = %q, want the one given", *coord)
	}
	if *base != "https://weft.example.com" || !*tls {
		t.Errorf("base-url = %q, redis-tls = %v, want the file's", *base, *tls)
	}
}

// With no file the flags are exactly what they were.
func TestOverlay_NoFileChangesNothing(t *testing.T) {
	t.Setenv(EnvPath, "")
	fs, coord, _, _ := apiFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := Overlay(fs, API, ""); err != nil {
		t.Fatal(err)
	}
	if *coord != "localhost:9025" {
		t.Errorf("--coordinator = %q, want the default", *coord)
	}
}

// $WEFT_CONFIG names the file when --config is not given.
func TestOverlay_ReadsTheEnvironmentPath(t *testing.T) {
	t.Setenv(EnvPath, write(t, file))
	fs, coord, _, _ := apiFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := Overlay(fs, API, ""); err != nil {
		t.Fatal(err)
	}
	if *coord != "localhost:9125" {
		t.Errorf("--coordinator = %q, want the file's", *coord)
	}
}

// A misspelled key is an error, not a silent default.
func TestLoad_RefusesAnUnknownKey(t *testing.T) {
	if _, err := Load(write(t, "regoin: us-east-2\n")); err == nil {
		t.Fatal("Load accepted an unknown key")
	}
}

// The environment is only filled where the server was not already given it.
func TestOverlay_EnvIsADefault(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	os.Unsetenv("AWS_REGION")
	fs, _, _, _ := apiFlags()
	if err := Overlay(fs, API, write(t, file)); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("AWS_REGION"); got != "eu-west-1" {
		t.Errorf("AWS_REGION = %q, want the file's", got)
	}
	t.Setenv("AWS_REGION", "us-west-2")
	if err := Overlay(fs, API, write(t, file)); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("AWS_REGION"); got != "us-west-2" {
		t.Errorf("AWS_REGION = %q, want the one already set", got)
	}
}
