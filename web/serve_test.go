package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestHandlerServesFilesAndFallsBackToTheApp(t *testing.T) {
	h := newHandler(fstest.MapFS{
		"index.html":      {Data: []byte("<app>")},
		"assets/app-1.js": {Data: []byte("js")},
	})
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w
	}
	if w := get("/assets/app-1.js"); w.Code != 200 || w.Body.String() != "js" || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("asset: %d %q %q", w.Code, w.Body, w.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/", "/dashboards/ops", "/cron/a/b"} {
		if w := get(p); w.Code != 200 || w.Body.String() != "<app>" || w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q %q", p, w.Code, w.Body, w.Header().Get("Cache-Control"))
		}
	}
	// A path of the api's that nothing answered is the api's 404, not the app.
	for _, p := range []string{"/api/nope", "/api"} {
		if w := get(p); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, w.Code)
		}
	}
}
