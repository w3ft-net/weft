package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/sources"
)

func TestDefaultProducers_AreTheHTTPSOnesTheMuxUsedToHardCode(t *testing.T) {
	r := DefaultProducers()
	if got := strings.Join(r.Names(), ","); got != "heroku,kinesis,otlp" {
		t.Fatalf("names = %s", got)
	}
	routes := map[string]string{}
	sources.Each(r, func(name string, p HTTPProducer) { routes[name] = p.Route() })
	want := map[string]string{
		"heroku":  "POST /api/ingest/heroku/{token}",
		"kinesis": "POST /api/ingest/cloudwatch",
		"otlp":    "POST /api/ingest/otlp/v1/metrics",
	}
	for name, route := range want {
		if routes[name] != route {
			t.Errorf("%s route = %q, want %q", name, routes[name], route)
		}
	}
}

// refusing is an HTTPProducer that refuses every request at Auth, so a request
// that reaches it proves its route was mounted.
type refusing struct {
	sources.Push
	route string
	seen  *int
}

func (r refusing) Route() string { return r.route }
func (r refusing) Auth(*http.Request) (string, error) {
	*r.seen++
	return "", http.ErrNoCookie
}
func (refusing) Parse(io.Reader, http.Header, AdapterContext) ([]*pb.LogRow, any, error) {
	return nil, nil, nil
}
func (refusing) ContentLimit() int64 { return 1 << 10 }

// A producer registered with the server mounts at the route it names, beside the
// core's, with no edit of the mux.
func TestHandler_MountsRegisteredHTTPProducers(t *testing.T) {
	srv := newOpenAPI(t).srv
	seen := 0
	if err := srv.Producers().Register("acme", refusing{route: "POST /api/ingest/acme", seen: &seen}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/ingest/acme", strings.NewReader("x")))
	if seen != 1 || rec.Code == http.StatusNotFound {
		t.Fatalf("the registered producer's route: status %d, Auth calls %d", rec.Code, seen)
	}

	// The core's are still there.
	for _, path := range []string{"/api/ingest/heroku/tok", "/api/ingest/cloudwatch", "/api/ingest/otlp/v1/metrics"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("x")))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s is not mounted", path)
		}
	}
}
