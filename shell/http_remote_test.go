//go:build !(js && wasm)

package shell

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
