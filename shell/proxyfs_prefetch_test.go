//go:build !(js && wasm)

package shell

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
)

// TestProxyFS_Prefetch_WalksCronD: crons live one directory down, at
// /cron.d/<namespace>/<name>, so warming /cron.d alone leaves every
// namespace listing cold. Prefetch warms /cron.d and everything under it with
// one walk request, not a listing per namespace.
func TestProxyFS_Prefetch_WalksCronD(t *testing.T) {
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.URL.Query().Get("path")+" walk="+r.URL.Query().Get("walk"))
		mu.Unlock()
		if r.URL.Query().Get("walk") == "1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"paths": []string{"alpha/", "alpha/errors", "beta/"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []any{}})
	}))
	defer srv.Close()

	NewProxyFS(srv.URL, "tok", "acme").Prefetch()

	sort.Strings(reqs)
	want := []string{"/bin walk=", "/cron.d walk=1", "/etc/dashboards walk="}
	if len(reqs) != len(want) {
		t.Fatalf("requests %v, want %v", reqs, want)
	}
	for i := range want {
		if reqs[i] != want[i] {
			t.Fatalf("requests %v, want %v", reqs, want)
		}
	}
}
