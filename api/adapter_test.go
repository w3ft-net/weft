//go:build !defaultshape

package api

import (
	"context"
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
	raw, _, err := s.CreateAgentToken(cust.ID, ns.ID, "adapter-test-token", "")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw, ns
}

// TestIngestHandler_EnrichesConfiguredService is the case the
// api-server-should-parse-like-weftd discussion was about: a service
// with a configured schema gets Level/TraceId/PayloadJson filled
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
	if err := s.AddLogGroup(context.Background(), ns.ID, store.SourceTypeDirect, "myapp", "", "us-east-2", schemaYAML); err != nil {
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

// herokuLines builds n single-frame-per-line Heroku body frames for service.
func herokuLines(service string, n int) string {
	frames := make([]string, n)
	for i := range frames {
		msg := fmt.Sprintf(`<134>1 2026-04-30T12:00:00.000000+00:00 host %s web.1 - - INFO req-%d hello`, service, i)
		frames[i] = herokuFrame(msg)
	}
	return hbody(frames...)
}

func postHeroku(t *testing.T, srv *Server, token, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.IngestHandler(HerokuAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

// TestIngestHandler_Heroku_SamplesSchemalessService: a raw-push service
// the coordinator auto-onboarded (configured, no schema) accumulates a
// sample from live traffic — same bootstrap weftd and kinesis get — while
// its rows keep flowing. A full sample ends the resample and surfaces the
// source for schema review.
func TestIngestHandler_Heroku_SamplesSchemalessService(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)
	if err := s.EnsureServiceLogGroup(ns.ID, "myapp"); err != nil {
		t.Fatal(err)
	}

	postHeroku(t, srv, token, herokuLines("myapp", 3))
	waitForRows(t, fake, 3)

	lg, err := s.FindLogGroup(ns.OrgID, ns.Name, "myapp")
	if err != nil || lg == nil {
		t.Fatalf("FindLogGroup = %v, %v", lg, err)
	}
	if lg.Status != store.StatusConfigured {
		t.Errorf("Status = %q, want configured — sampling must not gate ingest", lg.Status)
	}
	if lg.SampleCollected != 3 {
		t.Errorf("SampleCollected = %d, want 3", lg.SampleCollected)
	}
	if lg.ResampleRequestedAt == nil {
		t.Error("resample flag cleared early, want it held until the sample is full")
	}

	postHeroku(t, srv, token, herokuLines("myapp", store.SampleTarget))
	waitForRows(t, fake, 3+store.SampleTarget)

	lg, err = s.FindLogGroup(ns.OrgID, ns.Name, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if lg.SampleCollected != store.SampleTarget {
		t.Errorf("SampleCollected = %d, want %d (capped)", lg.SampleCollected, store.SampleTarget)
	}
	if lg.ResampleRequestedAt != nil {
		t.Error("resample flag still set after a full sample")
	}
	if n, _ := s.CountPendingLogGroups(ns.OrgID, []string{ns.Name}); n != 1 {
		t.Errorf("pending count = %d, want 1 — a full schemaless sample awaits review", n)
	}
}

// TestIngestHandler_Heroku_ConfiguredServiceNotSampled: a service with a
// schema and no resample request must not touch its sample.
func TestIngestHandler_Heroku_ConfiguredServiceNotSampled(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)
	if err := s.AddLogGroup(context.Background(), ns.ID, store.SourceTypeDirect, "myapp", "", "us-east-2", "app: myapp\nformat: text\n"); err != nil {
		t.Fatal(err)
	}

	postHeroku(t, srv, token, herokuLines("myapp", 3))
	waitForRows(t, fake, 3)

	lg, err := s.FindLogGroup(ns.OrgID, ns.Name, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if lg.SampleCollected != 0 || lg.Sample != "" {
		t.Errorf("sample = %q (%d), want none for a configured source", lg.Sample, lg.SampleCollected)
	}
}

// A brand-new push service has no log_group row when its first batch is
// ingested (the provisioner creates it after the rows are forwarded), so the
// first batch must create it rather than being dropped from the sample:
// otherwise a service that pushes exactly one sample's worth of lines in
// batches ends up with only the later batches.
func TestIngestHandler_Heroku_FirstBatchOfNewServiceIsSampled(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)

	postHeroku(t, srv, token, herokuLines("brandnew", 25))
	waitForRows(t, fake, 25)

	lg, err := s.FindLogGroup(ns.OrgID, ns.Name, "brandnew")
	if err != nil || lg == nil {
		t.Fatalf("FindLogGroup = %v, %v: the first batch must create the service's row", lg, err)
	}
	if lg.SampleCollected != 25 {
		t.Errorf("after the first batch SampleCollected = %d, want 25", lg.SampleCollected)
	}

	postHeroku(t, srv, token, herokuLines("brandnew", 25))
	waitForRows(t, fake, 50)
	lg, _ = s.FindLogGroup(ns.OrgID, ns.Name, "brandnew")
	if lg.SampleCollected != store.SampleTarget {
		t.Errorf("after two batches SampleCollected = %d, want %d", lg.SampleCollected, store.SampleTarget)
	}
}
