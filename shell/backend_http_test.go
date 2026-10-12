//go:build !(js && wasm)

package shell

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// recordedReq captures the relevant bits of an inbound request so a
// test can assert on what the backend actually sent (method, path,
// auth headers, body). One request per test exchange.
type recordedReq struct {
	method   string
	path     string
	rawQuery string
	auth     string
	org      string
	ctype    string
	body     string
}

// newTestBackend returns an HTTPBackend pointed at an httptest.Server
// driven by handler, plus a pointer the test can read after the call
// to inspect what the backend sent.
func newTestBackend(t *testing.T, handler func(rec *recordedReq, w http.ResponseWriter)) (*HTTPBackend, *recordedReq) {
	t.Helper()
	rec := &recordedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.rawQuery = r.URL.RawQuery
		rec.auth = r.Header.Get("Authorization")
		rec.org = r.Header.Get("X-Active-Org")
		rec.ctype = r.Header.Get("Content-Type")
		rec.body = string(body)
		handler(rec, w)
	}))
	t.Cleanup(srv.Close)
	return NewHTTPBackend(srv.URL, "tok-test", "acme"), rec
}

// TestNewHTTPBackend_StripsTrailingSlash: callers sometimes pass
// "https://api.test/" — the backend should normalize so the / in
// the path doesn't double up.
func TestNewHTTPBackend_StripsTrailingSlash(t *testing.T) {
	b := NewHTTPBackend("https://api.test/", "t", "o")
	if b.baseURL != "https://api.test" {
		t.Errorf("baseURL = %q, want trailing-/-stripped", b.baseURL)
	}
}

// TestHTTPBackend_SetAuthHeaders pins the contract that every request
// carries Bearer + X-Active-Org. Empty org omits the header rather
// than sending an empty value (the api uses presence as the signal).
func TestHTTPBackend_SetAuthHeaders(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Write([]byte("ok"))
	})
	if _, err := b.apiGet("/api/anything"); err != nil {
		t.Fatalf("apiGet: %v", err)
	}
	if rec.auth != "Bearer tok-test" {
		t.Errorf("Authorization = %q", rec.auth)
	}
	if rec.org != "acme" {
		t.Errorf("X-Active-Org = %q", rec.org)
	}

	// Empty org should omit the header.
	b2 := NewHTTPBackend("http://x", "t", "")
	// We can't easily inspect a request without a server, but the
	// setAuthHeaders branch is exercised below.
	req, _ := http.NewRequest("GET", "http://x/foo", nil)
	b2.setAuthHeaders(req)
	if got := req.Header.Get("X-Active-Org"); got != "" {
		t.Errorf("empty org should omit header; got %q", got)
	}
}

// TestURLQueryEscape spans the percent-encoding branches: alnum and
// the unreserved set pass through; everything else hex-encodes.
// "/" is intentionally unreserved here — paths are the typical input.
func TestURLQueryEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"a-z.0_9~", "a-z.0_9~"},
		{"/log/api.log", "/log/api.log"},
		{"hello world", "hello%20world"},
		{"a?b=c&d", "a%3Fb%3Dc%26d"},
		{"é", "%C3%A9"}, // multi-byte
	}
	for _, c := range cases {
		if got := urlQueryEscape(c.in); got != c.want {
			t.Errorf("urlQueryEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHTTPBackend_ListFunctions(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]any{
			"functions": []map[string]string{{"name": "scan"}, {"name": "billing-job"}},
		})
	})
	names, err := b.ListFunctions()
	if err != nil {
		t.Fatalf("ListFunctions: %v", err)
	}
	if rec.path != "/api/functions" {
		t.Errorf("path = %q", rec.path)
	}
	if len(names) != 2 || names[0] != "scan" || names[1] != "billing-job" {
		t.Errorf("got %v", names)
	}
}

func TestHTTPBackend_GetFunction(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Write([]byte("cat /log/foo | grep ERROR"))
	})
	src, err := b.GetFunction("foo")
	if err != nil {
		t.Fatalf("GetFunction: %v", err)
	}
	if rec.path != "/api/functions/foo" {
		t.Errorf("path = %q", rec.path)
	}
	if src != "cat /log/foo | grep ERROR" {
		t.Errorf("body = %q", src)
	}
}

func TestHTTPBackend_GetFunction_NotFound(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := b.GetFunction("missing"); err == nil {
		t.Error("expected error on 404")
	}
}

func TestHTTPBackend_SaveFunction(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
	})
	if err := b.SaveFunction("foo", "echo hi"); err != nil {
		t.Fatalf("SaveFunction: %v", err)
	}
	if rec.method != "PUT" || rec.path != "/api/functions/foo" {
		t.Errorf("got %s %s", rec.method, rec.path)
	}
	if rec.ctype != "application/json" {
		t.Errorf("Content-Type = %q", rec.ctype)
	}
	if !strings.Contains(rec.body, `"source":"echo hi"`) {
		t.Errorf("body = %q", rec.body)
	}
}

func TestHTTPBackend_SaveFunction_ServerError(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid pipeline"))
	})
	err := b.SaveFunction("foo", "garbage")
	if err == nil || !strings.Contains(err.Error(), "invalid pipeline") {
		t.Errorf("err = %v, want 'invalid pipeline'", err)
	}
}

func TestHTTPBackend_DeleteFunction(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := b.DeleteFunction("foo"); err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if rec.method != "DELETE" || rec.path != "/api/functions/foo" {
		t.Errorf("got %s %s", rec.method, rec.path)
	}
}

func TestHTTPBackend_GetWatch(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]any{
			"Schedule": "*/5 * * * *",
			"Pipeline": "cat /log/x | grep ERROR | wc -l | alert above 0",
			"Enabled":  true,
		})
	})
	w, err := b.GetWatch("error-count")
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if rec.path != "/api/crons/error-count" {
		t.Errorf("path = %q", rec.path)
	}
	if w.Schedule != "*/5 * * * *" || !w.Enabled {
		t.Errorf("watch = %+v", w)
	}
}

func TestHTTPBackend_SaveWatch(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {})
	wd := &WatchData{Schedule: "@hourly", Pipeline: "cat /log/x | wc -l"}
	if err := b.SaveWatch("foo", wd); err != nil {
		t.Fatalf("SaveWatch: %v", err)
	}
	if !strings.Contains(rec.body, `"schedule":"@hourly"`) {
		t.Errorf("body missing schedule: %q", rec.body)
	}
}

// TestHTTPBackend_SaveWatch_RelaysWarnings: a 200 response carrying a
// "warnings" field is not a failure — SaveWatch must still return an
// error (so the generic store.Split-based callers can display it),
// but store.Split on that error must show zero blocking errors.
func TestHTTPBackend_SaveWatch_RelaysWarnings(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"warnings": []string{"tail -n 1000 caps this cron at 1000 rows per run"},
		})
	})
	wd := &WatchData{Schedule: "*/5 * * * *", Pipeline: "tail -n 1000 x | grep -c y"}
	err := b.SaveWatch("risky", wd)
	if err == nil {
		t.Fatal("SaveWatch: want a non-nil error carrying the warning, got nil")
	}
	blocking, warnings := store.Split(err)
	if len(blocking) != 0 {
		t.Fatalf("SaveWatch: got blocking error %v, want the save to have succeeded", blocking)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tail -n 1000") {
		t.Errorf("warnings: got %v, want one message naming the cap", warnings)
	}
}

// TestHTTPBackend_SaveWatch_NoWarningsFieldIsClean: a plain 200 with
// no "warnings" field (or an empty one) must not synthesize a
// warning out of nothing.
func TestHTTPBackend_SaveWatch_NoWarningsFieldIsClean(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	wd := &WatchData{Schedule: "@hourly", Pipeline: "cat /log/x | wc -l"}
	if err := b.SaveWatch("clean", wd); err != nil {
		t.Fatalf("SaveWatch: got %v, want nil", err)
	}
}

// TestHTTPBackend_GetNotifyConfig_NotFoundIsEmpty: an unset notify
// config yields 404 → empty string, not an error. The shell treats
// "you haven't set up aliases" as a normal state.
func TestHTTPBackend_GetNotifyConfig_NotFoundIsEmpty(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
	})
	got, err := b.GetNotifyConfig()
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestHTTPBackend_GetNotifyConfig_Decodes(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]string{"aliases_yaml": "page: oncall\n"})
	})
	got, err := b.GetNotifyConfig()
	if err != nil {
		t.Fatalf("GetNotifyConfig: %v", err)
	}
	if got != "page: oncall\n" {
		t.Errorf("got %q", got)
	}
}

func TestHTTPBackend_ListDashboards(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]any{
			"dashboards": []map[string]string{{"name": "prod"}, {"name": "staging"}},
		})
	})
	names, err := b.ListDashboards()
	if err != nil {
		t.Fatalf("ListDashboards: %v", err)
	}
	if len(names) != 2 || names[0] != "prod" {
		t.Errorf("got %v", names)
	}
}

func TestHTTPBackend_GetDashboard(t *testing.T) {
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]string{"yaml_body": "charts: [foo]\n"})
	})
	body, err := b.GetDashboard("prod")
	if err != nil {
		t.Fatalf("GetDashboard: %v", err)
	}
	if body != "charts: [foo]\n" {
		t.Errorf("body = %q", body)
	}
}

func TestHTTPBackend_GetWatchResults_LimitInQuery(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		json.NewEncoder(w).Encode([]map[string]string{
			{"Stdout": "42", "RanAt": "2026-04-01T12:00:00Z"},
			{"Stdout": "  17  ", "RanAt": "2026-04-01T13:00:00Z"},
		})
	})
	rows, err := b.GetWatchResults("err", 50)
	if err != nil {
		t.Fatalf("GetWatchResults: %v", err)
	}
	if rec.rawQuery != "limit=50" {
		t.Errorf("query = %q", rec.rawQuery)
	}
	if len(rows) != 2 || rows[0].Value != "42" || rows[1].Value != "17" {
		t.Errorf("got %+v", rows)
	}
}

// TestHTTPBackend_GetWatchResults_NoLimit: limit=0 omits the query
// param so the API applies its server-side default. Pins this
// because a stray `?limit=0` would force a hard cap of 0 results.
func TestHTTPBackend_GetWatchResults_NoLimit(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Write([]byte("[]"))
	})
	if _, err := b.GetWatchResults("x", 0); err != nil {
		t.Fatalf("GetWatchResults: %v", err)
	}
	if rec.rawQuery != "" {
		t.Errorf("expected no query string, got %q", rec.rawQuery)
	}
}

func TestHTTPBackend_GetFile_QueryEscaped(t *testing.T) {
	b, rec := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Write([]byte("file contents"))
	})
	data, err := b.GetFile("/log/foo bar/baz.log")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if string(data) != "file contents" {
		t.Errorf("data = %q", data)
	}
	// Spaces escaped, slashes preserved.
	if !strings.Contains(rec.rawQuery, "path=/log/foo%20bar/baz.log") {
		t.Errorf("rawQuery = %q", rec.rawQuery)
	}
}

func TestHTTPBackend_ApiListNames_NoEnvelopeReturnsEmpty(t *testing.T) {
	// If the server response doesn't unmarshal to the expected envelope,
	// apiListNames returns an empty slice (not an error). This is the
	// completion path's "no functions defined" state.
	b, _ := newTestBackend(t, func(_ *recordedReq, w http.ResponseWriter) {
		w.Write([]byte("not json"))
	})
	names, err := b.apiListNames("/api/functions", "functions")
	if err != nil {
		t.Fatalf("apiListNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("got %v, want empty", names)
	}
}
