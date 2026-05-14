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
// ship the raw spec verbatim under the "range" key and must NOT send the
// legacy nanos fields (the API server no longer reads them). Before the
// server-side-resolution rework the CLI would silently drop the time range
// entirely (signature took start/end but never serialized them), so the
// coordinator scanned every shard — this test guards against regressing to
// that.
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
	if _, err := c.Query("cat /log/foo", "1d.."); err != nil {
		t.Fatalf("Query: %v", err)
	}

	if got := gotBody["range"]; got != "1d.." {
		t.Errorf("body[range] = %v, want %q", got, "1d..")
	}
	if _, ok := gotBody["time_start_ns"]; ok {
		t.Errorf("time_start_ns must not be in body anymore: %v", gotBody)
	}
	if _, ok := gotBody["time_end_ns"]; ok {
		t.Errorf("time_end_ns must not be in body anymore: %v", gotBody)
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
	_, err := c.Query("cat /log/foo", "1d..")
	if err == nil {
		t.Fatal("expected error for 426 response")
	}
	msg := err.Error()
	if !strings.Contains(msg, "too old") || !strings.Contains(msg, "0.3.0") || !strings.Contains(msg, "weft update") {
		t.Errorf("error %q missing one of: 'too old', required version, upgrade hint", msg)
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
	if _, err := c.Query("cat /log/foo", "1d.."); err != nil {
		t.Fatalf("Query: %v", err)
	}

	if !strings.HasPrefix(gotHeader, "weft/") {
		t.Errorf("X-Weft-Client = %q, want prefix %q", gotHeader, "weft/")
	}
	if gotHeader != "weft/0.4.2+abc123" {
		t.Errorf("X-Weft-Client = %q, want %q", gotHeader, "weft/0.4.2+abc123")
	}
}
