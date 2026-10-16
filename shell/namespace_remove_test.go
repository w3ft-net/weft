package shell

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// RemoveNamespace sends force as a query flag, and turns the server's 409 into
// a typed error that carries what the namespace still holds.
func TestRemoveNamespace(t *testing.T) {
	var gotPath, gotForce string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotForce = r.URL.Path, r.URL.Query().Get("force")
		if gotForce != "true" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"namespace \"ns\" is not empty","sources":["api","web"],"tokens":2}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	h := NewHTTPCoordinator(srv.URL, "tok", "")

	err := h.RemoveNamespace("ns", false)
	var notEmpty *NamespaceNotEmptyError
	if !errors.As(err, &notEmpty) {
		t.Fatalf("RemoveNamespace on a non-empty namespace = %v, want *NamespaceNotEmptyError", err)
	}
	if len(notEmpty.Sources) != 2 || notEmpty.Sources[1] != "web" || notEmpty.Tokens != 2 {
		t.Errorf("error carries %+v, want sources [api web] and 2 tokens", notEmpty)
	}
	if gotPath != "/api/namespaces/ns" || gotForce != "" {
		t.Errorf("request = %s force=%q, want /api/namespaces/ns with no force", gotPath, gotForce)
	}

	if err := h.RemoveNamespace("ns", true); err != nil {
		t.Fatalf("forced RemoveNamespace = %v", err)
	}
	if gotForce != "true" {
		t.Errorf("force query = %q, want true", gotForce)
	}
}
