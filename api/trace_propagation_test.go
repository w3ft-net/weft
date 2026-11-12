//go:build !defaultshape

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"
)

// TestIngestHandler_ForwardsTraceIDToCoordinator proves the request's
// trace ID rides the IngestStream gRPC call as `weft-trace-id` metadata
// (context.Value does not cross gRPC on its own), so the coordinator's
// logs for an HTTP-pushed batch join the request's trace.
func TestIngestHandler_ForwardsTraceIDToCoordinator(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient
	token, _ := setupIngestOrg(t, s)

	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - hello`
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(hbody(herokuFrame(msg))))
	req = req.WithContext(weftsdk.ContextWithTraceID(context.Background(), "ingest-trace-123"))
	w := httptest.NewRecorder()
	srv.IngestHandler(HerokuAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	waitForRows(t, fake, 1)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.traceSeen) != 1 || fake.traceSeen[0] != "ingest-trace-123" {
		t.Fatalf("weft-trace-id metadata seen by coordinator = %v, want [ingest-trace-123]", fake.traceSeen)
	}
}
