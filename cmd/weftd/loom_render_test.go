package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The loom-producer host renders its weftd config via
// deploy/loom-producer/render-weftd-config.sh. After the weftd 0.20.0
// upgrade two things must hold for loom's logs to land at
// loom/{api,worker,edge} rather than as filename-named pending sources
// in whatever namespace the *fleet* WEFTD_TOKEN points at:
//
//  1. Sources must use the 0.20.0 `service:` key. The pre-0.20.0 `app:`
//     key is silently ignored (lenient YAML), leaving Service empty so
//     weftd falls back to basename(path) → "api-prod" etc.
//  2. The render must emit a token.env that the unit loads *after*
//     /opt/app/.env, so the loom token wins the env-token chain
//     (main.go:197, tokenFromChain — env WEFTD_TOKEN beats the file's
//     token:). Otherwise the fleet WEFTD_TOKEN that load-app-env drops
//     into /opt/app/.env hijacks the forwarder onto the ops namespace.
//
// This test runs the real script and pins both invariants.
func TestLoomRenderConfig(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"deploy", "loom-producer", "render-weftd-config.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("render script not found: %v", err)
	}

	const (
		loomProdToken = "wft_loomprod00000000000000000001"
		fleetToken    = "wft_fleetops00000000000000000002" // the /opt/app/.env WEFTD_TOKEN
	)

	confDir := t.TempDir()
	cmd := exec.Command("bash", script, "prod")
	cmd.Env = append(os.Environ(),
		"WEFTD_LOOM_CONFDIR="+confDir,
		"LOOM_PROD_TOKEN="+loomProdToken,
		// Simulate the fleet token leaking in via EnvironmentFile=/opt/app/.env.
		"WEFTD_TOKEN="+fleetToken,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render script failed: %v\n%s", err, out)
	}

	// --- invariant 1: source names survive as bare services ---
	data, err := os.ReadFile(filepath.Join(confDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read rendered config.yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("rendered config.yaml does not parse: %v", err)
	}

	want := []struct{ service, path string }{
		{"api", "/var/log/loom/api-prod.log"},
		{"worker", "/var/log/loom/worker-prod.log"},
		{"edge", "/var/log/loom/edge-prod.log"},
	}
	if len(cfg.Sources) != len(want) {
		t.Fatalf("got %d sources, want %d: %+v", len(cfg.Sources), len(want), cfg.Sources)
	}
	for i, w := range want {
		got := cfg.Sources[i]
		// effectiveService mirrors main.go's derivation: Service wins,
		// else basename(path). A config still on `app:` lands here with
		// Service=="" and silently becomes "api-prod".
		if got.Service != w.service {
			t.Errorf("source %d: Service = %q, want %q (config still on `app:`? it renders as basename %q)",
				i, got.Service, w.service, basenameService(got.Path))
		}
		if got.Path != w.path {
			t.Errorf("source %d: Path = %q, want %q", i, got.Path, w.path)
		}
	}

	// --- invariant 2: token.env carries the loom token and wins ---
	tokenEnv := filepath.Join(confDir, "token.env")
	envMap, err := parseEnvFile(tokenEnv)
	if err != nil {
		t.Fatalf("read rendered token.env: %v (the render must emit it so the unit can override the fleet WEFTD_TOKEN)", err)
	}
	if got := envMap["WEFTD_TOKEN"]; got != loomProdToken {
		t.Fatalf("token.env WEFTD_TOKEN = %q, want the loom token %q", got, loomProdToken)
	}

	// The unit loads /opt/app/.env (WEFTD_TOKEN=fleet) then token.env
	// (WEFTD_TOKEN=loom); the later file wins. Confirm the token weftd
	// actually authenticates with is the loom token, not the fleet one.
	effectiveEnvToken := envMap["WEFTD_TOKEN"] // token.env overrides /opt/app/.env
	if got := tokenFromChain(effectiveEnvToken, cfg.Token); got != loomProdToken {
		t.Fatalf("effective auth token = %q, want loom %q (fleet token would route loom logs to the ops namespace)",
			got, loomProdToken)
	}
}

// The token.env override only works if the units are wired so the
// loom token wins over the fleet WEFTD_TOKEN. Pin that wiring: the
// forwarder must load /opt/app/.env first and token.env second, must
// fail closed when token.env is missing (no '-' prefix), and must
// order/require the renderer unit that produces token.env.
func TestLoomUnitWiring(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "loom-producer")

	forwarder := readFile(t, filepath.Join(dir, "weftd-loom@.service"))

	dotEnv := indexOfLine(forwarder, "EnvironmentFile=/opt/app/.env")
	tokenEnv := indexOfLine(forwarder, "EnvironmentFile=/etc/weftd-loom-%i/token.env")
	if dotEnv < 0 {
		t.Fatal("weftd-loom@.service must load EnvironmentFile=/opt/app/.env")
	}
	if tokenEnv < 0 {
		t.Fatal("weftd-loom@.service must load EnvironmentFile=/etc/weftd-loom-%i/token.env")
	}
	if !(dotEnv < tokenEnv) {
		t.Fatal("token.env must be loaded AFTER /opt/app/.env so the loom token overrides the fleet WEFTD_TOKEN")
	}
	// Fail closed: a missing token.env must error, never fall back to the
	// fleet token. The '-' prefix would make systemd ignore it if absent.
	if strings.Contains(forwarder, "EnvironmentFile=-/etc/weftd-loom-%i/token.env") {
		t.Fatal("token.env EnvironmentFile must not use the '-' (ignore-if-missing) prefix — fail closed")
	}
	if !strings.Contains(forwarder, "Requires=weftd-loom-config@%i.service") {
		t.Fatal("weftd-loom@.service must Requires= the renderer so it fails closed if the render fails")
	}
	if !strings.Contains(forwarder, "After=") || !strings.Contains(forwarder, "weftd-loom-config@%i.service") {
		t.Fatal("weftd-loom@.service must order After= weftd-loom-config@%i.service")
	}

	renderer := readFile(t, filepath.Join(dir, "weftd-loom-config@.service"))
	if !strings.Contains(renderer, "render-weftd-config.sh %i") {
		t.Fatal("weftd-loom-config@.service must ExecStart the render script with %i")
	}
	if !strings.Contains(renderer, "Type=oneshot") {
		t.Fatal("weftd-loom-config@.service must be Type=oneshot")
	}
	// No RemainAfterExit: each (re)start of the forwarder must re-render.
	if strings.Contains(renderer, "RemainAfterExit=yes") {
		t.Fatal("weftd-loom-config@.service must not RemainAfterExit — it should re-run on each forwarder (re)start")
	}
}

// TestLoomUnitsInstalledByPacker pins that the AMI build installs every
// loom-producer systemd unit. The tests above verify unit *content* in the
// repo, but the service:/token.env fix never took hold because the packer
// template omitted weftd-loom-config@.service — so the forwarder failed
// closed on its Requires= and the host kept shipping under the fleet token.
// A correct repo unit that isn't baked onto the image is worthless; guard it.
func TestLoomUnitsInstalledByPacker(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")

	entries, err := os.ReadDir(filepath.Join(root, "deploy", "loom-producer"))
	if err != nil {
		t.Fatalf("read loom-producer dir: %v", err)
	}
	pkr := readFile(t, filepath.Join(root, "packer", "weft.pkr.hcl"))

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".service") {
			continue
		}
		if !strings.Contains(pkr, "deploy/loom-producer/"+name) {
			t.Errorf("packer template never provisions %s — it won't exist on the AMI", name)
		}
		// The unit must be moved into /etc/systemd/system. Find an install
		// line that names this unit's staged /tmp path.
		installed := false
		for _, line := range strings.Split(pkr, "\n") {
			if strings.Contains(line, "/etc/systemd/system") && strings.Contains(line, "/tmp/"+name) {
				installed = true
				break
			}
		}
		if !installed {
			t.Errorf("packer template never installs %s into /etc/systemd/system", name)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// indexOfLine returns the byte offset of the first line that, trimmed,
// equals want — or -1. Used to compare EnvironmentFile ordering.
func indexOfLine(content, want string) int {
	off := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == want {
			return off
		}
		off += len(line) + 1
	}
	return -1
}

// basenameService mirrors main.go's path→service fallback, for a clearer
// failure message when a source is still authored with `app:`.
func basenameService(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// parseEnvFile reads a KEY=VALUE env file (systemd EnvironmentFile shape),
// stripping optional surrounding quotes.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}
