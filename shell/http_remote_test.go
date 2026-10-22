//go:build !(js && wasm)

package shell

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPCoordinator_QuerySendsRange pins the CLI body shape: Query must
// ship the raw spec verbatim under the "range" key; dropping it would make
// the coordinator scan every shard.
func TestHTTPCoordinator_QuerySendsRange(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer srv.Close()

	c := NewHTTPCoordinator(srv.URL, "tok", "org")
	if _, err := c.Query("cat api.log", "1d..", []string{"/log/acme", "/tmp"}); err != nil {
		t.Fatalf("Query: %v", err)
	}

	if got := gotBody["range"]; got != "1d.." {
		t.Errorf("body[range] = %v, want %q", got, "1d..")
	}
	// Dirstack courier: the cached stack must ride in the request body
	// (index 0 == cwd) so the server resolves relative paths against it.
	ds, ok := gotBody["dirstack"].([]any)
	if !ok || len(ds) != 2 || ds[0] != "/log/acme" || ds[1] != "/tmp" {
		t.Errorf("body[dirstack] = %v, want [/log/acme /tmp]", gotBody["dirstack"])
	}
}

// TestHTTPCoordinator_Query426 pins the friendly 426-handling path. When
// the server says "client too old" the CLI must produce a clear upgrade
// prompt, not a raw "HTTP 426: ..." dump. The user has to know what to do.
func TestHTTPCoordinator_Query426(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(426)
		_, _ = w.Write([]byte(`{"error":"client too old","running":"0.2.5","required":"0.3.0","upgrade":"run ` + "`weft update`" + `"}`))
	}))
	defer srv.Close()

	c := NewHTTPCoordinator(srv.URL, "tok", "org")
	c.SetClientID("weft/0.2.5")
	_, err := c.Query("cat /log/foo", "1d..", nil)
	if err == nil {
		t.Fatal("expected error for 426 response")
	}
	msg := err.Error()
	if !strings.Contains(msg, "too old") || !strings.Contains(msg, "0.3.0") || !strings.Contains(msg, "weft update") {
		t.Errorf("error %q missing one of: 'too old', required version, upgrade hint", msg)
	}
}

// TestHTTPCoordinator_ListApps401 pins the friendly 401-handling path.
// ListApps is what `weft sh` calls first (connectRemote, cmd/weft/main.go)
// to build the shell, so an expired/invalid token surfaces here — the CLI
// must say to run `weft login`, not dump a raw "HTTP 401" at the user.
func TestHTTPCoordinator_ListApps401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized: token expired", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewHTTPCoordinator(srv.URL, "stale-tok", "org")
	_, err := c.ListApps()
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	msg := err.Error()
	if strings.Contains(msg, "401") {
		t.Errorf("error %q leaks the raw HTTP status — want a login hint instead", msg)
	}
	if !strings.Contains(msg, "weft login") {
		t.Errorf("error %q missing a `weft login` hint", msg)
	}
}

// TestHTTPCoordinator_SendsClientHeader pins that every outbound request
// declares the binary's identity in X-Weft-Client. The server gate (HTTP
// 426 for old CLIs) is useless without it.
func TestHTTPCoordinator_SendsClientHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Weft-Client")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer srv.Close()

	c := NewHTTPCoordinator(srv.URL, "tok", "org")
	c.SetClientID("weft/0.4.2+abc123")
	if _, err := c.Query("cat /log/foo", "1d..", nil); err != nil {
		t.Fatalf("Query: %v", err)
	}

	if !strings.HasPrefix(gotHeader, "weft/") {
		t.Errorf("X-Weft-Client = %q, want prefix %q", gotHeader, "weft/")
	}
	if gotHeader != "weft/0.4.2+abc123" {
		t.Errorf("X-Weft-Client = %q, want %q", gotHeader, "weft/0.4.2+abc123")
	}
}
