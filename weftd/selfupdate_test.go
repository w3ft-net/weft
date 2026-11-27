package weftd

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The key the test "releases" are signed with, and the one a checker trusts.
var testPub, testPriv, _ = ed25519.GenerateKey(nil)

// serveSigned writes the manifest and its detached signature.
func serveSigned(w http.ResponseWriter, r *http.Request, m map[string]any, priv ed25519.PrivateKey) {
	body, _ := json.Marshal(m)
	if r.URL.Path == "/manifest.json.sig" {
		w.Write([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))))
		return
	}
	w.Write(body)
}

// newBinary is the bytes the test "release" serves; distinct from the
// old binary so a successful swap is observable byte-for-byte.
var newBinary = []byte("#!/fake/weftd new build\n")

// updateServer stands up a manifest + binary endpoint mimicking
// releases.w3ft.net. version is what the manifest advertises; if
// badSHA256 is set, the manifest lies about the binary's digest so the
// verify step must reject it.
func updateServer(t *testing.T, version string, badSHA256 bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(newBinary)
	sha := hex.EncodeToString(sum[:])
	if badSHA256 {
		sha = "deadbeef"
	}
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		m := map[string]any{
			"binary":  "weftd",
			"version": version,
			"platforms": []map[string]string{{
				"suffix": "test",
				"goos":   runtime.GOOS,
				"goarch": runtime.GOARCH,
				"sha256": sha,
				"url":    base + "/bin",
			}},
		}
		serveSigned(w, r, m, testPriv)
	})
	mux.HandleFunc("/manifest.json.sig", func(w http.ResponseWriter, r *http.Request) {
		serveSigned(w, r, map[string]any{
			"binary":  "weftd",
			"version": version,
			"platforms": []map[string]string{{
				"suffix": "test",
				"goos":   runtime.GOOS,
				"goarch": runtime.GOARCH,
				"sha256": sha,
				"url":    base + "/bin",
			}},
		}, testPriv)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Write(newBinary)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// checkerFor wires a SelfUpdateChecker at the test server with a temp
// "binary" seeded with old content and a Restart spy. Returns the
// checker, the binary path, and a pointer the spy flips.
func checkerFor(t *testing.T, srv *httptest.Server, current string, autoApply bool) (*SelfUpdateChecker, string, *bool) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "weftd")
	if err := os.WriteFile(bin, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	restarted := false
	c := &SelfUpdateChecker{
		CurrentVersion: current,
		AutoApply:      autoApply,
		ManifestURL:    srv.URL + "/manifest.json",
		PublicKey:      testPub,
		HTTP:           srv.Client(),
		ExecPath:       func() (string, error) { return bin, nil },
		Restart:        func() { restarted = true },
	}
	return c, bin, &restarted
}

func binContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSelfUpdateAutoApply(t *testing.T) {
	srv := updateServer(t, "0.19.0", false)
	c, bin, restarted := checkerFor(t, srv, "0.18.0", true)

	c.checkOnce()

	if got := binContent(t, bin); got != string(newBinary) {
		t.Fatalf("binary not replaced: got %q, want %q", got, newBinary)
	}
	if !*restarted {
		t.Fatal("Restart was not called after a successful swap")
	}
}

func TestSelfUpdateNagOnlyWhenAutoApplyOff(t *testing.T) {
	srv := updateServer(t, "0.19.0", false)
	c, bin, restarted := checkerFor(t, srv, "0.18.0", false)

	c.checkOnce()

	if got := binContent(t, bin); got != "OLD BINARY" {
		t.Fatalf("binary was replaced despite AutoApply=false: %q", got)
	}
	if *restarted {
		t.Fatal("Restart called despite AutoApply=false")
	}
}

func TestSelfUpdateNoNewerVersion(t *testing.T) {
	srv := updateServer(t, "0.18.0", false) // == current
	c, bin, restarted := checkerFor(t, srv, "0.18.0", true)

	c.checkOnce()

	if got := binContent(t, bin); got != "OLD BINARY" {
		t.Fatalf("binary replaced when not newer: %q", got)
	}
	if *restarted {
		t.Fatal("Restart called when no newer version")
	}
}

func TestSelfUpdateRejectsBadSHA(t *testing.T) {
	srv := updateServer(t, "0.19.0", true) // manifest lies about the digest
	c, bin, restarted := checkerFor(t, srv, "0.18.0", true)

	c.checkOnce()

	if got := binContent(t, bin); got != "OLD BINARY" {
		t.Fatalf("binary replaced despite sha256 mismatch: %q", got)
	}
	if *restarted {
		t.Fatal("Restart called despite sha256 mismatch")
	}
}

// Guard the platform-miss branch: a manifest with no build for this host
// must not replace anything, even with AutoApply on.
func TestSelfUpdateNoMatchingPlatform(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := map[string]any{
			"binary":  "weftd",
			"version": "0.19.0",
			"platforms": []map[string]string{{
				"suffix": "other", "goos": "plan9", "goarch": "mips",
				"sha256": "x", "url": "http://unused/bin",
			}},
		}
		serveSigned(w, r, m, testPriv)
	}))
	t.Cleanup(srv.Close)
	c, bin, restarted := checkerFor(t, srv, "0.18.0", true)

	c.checkOnce()

	if got := binContent(t, bin); got != "OLD BINARY" {
		t.Fatalf("binary replaced with no matching platform: %q", got)
	}
	if *restarted {
		t.Fatal("Restart called with no matching platform")
	}
}

// A manifest signed by any key but the configured one is refused, so a host
// pointed at a compromised or spoofed release server never swaps its binary.
func TestSelfUpdateRejectsWrongKey(t *testing.T) {
	srv := updateServer(t, "0.19.0", false)
	c, bin, restarted := checkerFor(t, srv, "0.18.0", true)
	other, _, _ := ed25519.GenerateKey(nil)
	c.PublicKey = other

	c.checkOnce()

	if got := binContent(t, bin); got != "OLD BINARY" {
		t.Fatalf("binary replaced from a manifest the key does not verify: %q", got)
	}
	if *restarted {
		t.Fatal("Restart called for an unverified manifest")
	}
}
