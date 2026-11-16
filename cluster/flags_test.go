package cluster_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/cluster"
	apioptions "github.com/w3ft-net/weft/cmd/api/server/options"
	coordoptions "github.com/w3ft-net/weft/cmd/coordinator/server/options"
)

// A cluster file can only default a flag the server has: a flag renamed in the
// api or the coordinator would otherwise silently stop being filled from it.
func TestFlags_NameFlagsTheServersHave(t *testing.T) {
	var c cluster.Config
	c.Region, c.BaseURL = "r", "https://x"
	c.Redis.Addr, c.Redis.TLS = "h:1", true
	c.Coordinator.Host, c.Coordinator.LocalPort, c.Coordinator.InternalPort = "c", 1, 2
	c.Compute.Host, c.Compute.Port = "w", 3

	for server, define := range map[string]func(*flag.FlagSet){
		cluster.API:         func(fs *flag.FlagSet) { apioptions.Define(fs) },
		cluster.Coordinator: func(fs *flag.FlagSet) { coordoptions.Define(fs) },
	} {
		fs := flag.NewFlagSet(server, flag.ContinueOnError)
		define(fs)
		flags := c.Flags(server)
		if len(flags) == 0 {
			t.Errorf("%s: the file defaults no flags", server)
		}
		for name := range flags {
			if fs.Lookup(name) == nil {
				t.Errorf("%s: the cluster file defaults --%s, which the server does not have", server, name)
			}
		}
		// Applied, the values land in the server's own fields.
		if err := cluster.Overlay(fs, server, writeFile(t, "base_url: https://y\nredis:\n  addr: z:2\n")); err != nil {
			t.Errorf("%s: %v", server, err)
		}
		if got := fs.Lookup("redis-addr").Value.String(); got != "z:2" {
			t.Errorf("%s: --redis-addr = %q after the overlay, want z:2", server, got)
		}
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "weft.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
