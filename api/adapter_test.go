package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// setupIngestOrg creates an org + namespace + generic (non-producer-
// scoped) agent token — the same shape HerokuAdapter and the future
// CloudWatchAdapter authenticate against. Returns the raw token and
// the namespace so callers can add log_groups rows scoped to it.
func setupIngestOrg(t *testing.T, s *store.Store) (token string, ns *store.Namespace) {
	t.Helper()
	cust, err := s.FindOrCreateOrg(fmt.Sprintf("adapter-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	ns, err = s.FindOrCreateNamespace(cust.ID, "test")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	raw, _, err := s.CreateAgentToken(cust.ID, ns.ID, "adapter-test-token", "", "")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw, ns
}

// TestIngestHandler_EnrichesConfiguredService is the case the
// api-server-should-parse-like-weftd discussion was about: a service
// with a configured schema gets Level/Fields/TraceId/PayloadJson filled
// in by enrichRows before the row ever reaches the coordinator's
// IngestStream — matching what weftd does client-side and what the
// CloudWatch poller does before shard.Ingest, neither of which
// IngestStream re-parses on its own.
func TestIngestHandler_EnrichesConfiguredService(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)

	// level_field=1, trace_id_field=2: "ERROR req-42 db timeout" →
	// Level="ERROR", TraceId="req-42".
	schemaYAML := "app: myapp\nformat: text\nlevel_field: \"1\"\ntrace_id_field: \"2\"\n"
	if err := s.AddLogGroup(ns.ID, store.SourceTypeDirect, "myapp", "", "us-east-2", schemaYAML); err != nil {
		t.Fatalf("add log group: %v", err)
	}

	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - ERROR req-42 db timeout`
	body := hbody(herokuFrame(msg))

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.IngestHandler(HerokuAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}

	rows := waitForRows(t, fake, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Service != "myapp" || row.Kind != "web" {
		t.Fatalf("row service/kind = %s/%s, want myapp/web", row.Service, row.Kind)
	}
	if row.Level != "ERROR" {
		t.Errorf("Level = %q, want ERROR — enrichRows should have applied myapp's schema", row.Level)
	}
	if row.TraceId != "req-42" {
		t.Errorf("TraceId = %q, want req-42", row.TraceId)
	}
	if len(row.Fields) == 0 {
		t.Error("Fields is empty; enrichRows should have populated it from the schema")
	}
	// The adapter's own syslog-derived Instance must survive enrichment
	// unclobbered — the schema has no envelope config, so ParseLine's
	// own Instance extraction would be empty and must not overwrite it.
	if row.Instance != "web.1" {
		t.Errorf("Instance = %q, want web.1 (must not be clobbered by schema enrichment)", row.Instance)
	}
}

// TestIngestHandler_PassthroughWhenServiceNotConfigured is the no-
// regression case: a service with no log_groups row (never onboarded)
// or no schema yet must pass through Raw-only, exactly like today —
// enrichRows must not error or block ingestion just because a schema
// isn't configured.
func TestIngestHandler_PassthroughWhenServiceNotConfigured(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, _ := setupIngestOrg(t, s)
	// Deliberately no AddLogGroup call — "unconfigured" service.

	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host neverconfigured web.1 - - plain message`
	body := hbody(herokuFrame(msg))

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.IngestHandler(HerokuAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}

	rows := waitForRows(t, fake, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Level != "" {
		t.Errorf("Level = %q, want empty — no schema configured for this service", rows[0].Level)
	}
	if rows[0].Raw != "plain message" {
		t.Errorf("Raw = %q, want %q", rows[0].Raw, "plain message")
	}
}
