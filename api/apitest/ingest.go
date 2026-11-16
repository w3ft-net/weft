package apitest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/sources"
)

// Ingest runs the HTTP ingest handlers (a Heroku drain, a CloudWatch Firehose
// delivery) against a fake coordinator: a token authenticates the push, a
// service with a schema is enriched, one without passes through, and a source
// still collecting its first sample fills it from live traffic.
func Ingest(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"EnrichesConfiguredService", ingestEnrichesConfiguredService},
		{"PassthroughWhenServiceNotConfigured", ingestPassthroughWhenServiceNotConfigured},
		{"Heroku_SamplesSchemalessService", ingestHerokuSamplesSchemalessService},
		{"Heroku_ConfiguredServiceNotSampled", ingestHerokuConfiguredServiceNotSampled},
		{"Heroku_FirstBatchOfNewServiceIsSampled", ingestHerokuFirstBatchOfNewServiceIsSampled},
		{"CloudWatch_GzipAndPlain", ingestCloudWatchGzipAndPlain},
		{"CloudWatch_ControlMessage", ingestCloudWatchControlMessage},
		{"CloudWatch_UnknownLogGroupDropped", ingestCloudWatchUnknownLogGroupDropped},
		{"CloudWatch_Unauthorized", ingestCloudWatchUnauthorized},
		{"CloudWatch_AccumulatesPendingSample", ingestCloudWatchAccumulatesPendingSample},
		{"CloudWatch_ConfiguredSourceNotResampled", ingestCloudWatchConfiguredSourceNotResampled},
		{"ForwardsTraceIDToCoordinator", ingestForwardsTraceID},
	})
}

// nsRef is a namespace by id and name.
type nsRef struct {
	ID   uint
	Name string
}

// ingestToken is a generic (not producer-scoped) agent token for namespace
// "test": the shape the Heroku and CloudWatch adapters authenticate against.
func (s Subject) ingestToken(t *testing.T) (string, nsRef) {
	t.Helper()
	id := s.namespace(t, "test")
	raw, _, err := s.Tokens.CreateToken(s.Owner, id, "adapter-test-token", "")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw, nsRef{ID: id, Name: "test"}
}

func (s Subject) addSource(t *testing.T, ns uint, kind, service, locator, schemaYAML string) {
	t.Helper()
	spec := source.SourceSpec{Locator: locator, Params: map[string]string{"region": "us-east-2"}}
	if err := s.Sources.AddSource(context.Background(), ns, kind, service, spec, schemaYAML); err != nil {
		t.Fatalf("add source: %v", err)
	}
}

func (s Subject) registerSource(t *testing.T, ns uint, kind, service, locator string) {
	t.Helper()
	spec := source.SourceSpec{Locator: locator, Params: map[string]string{"region": "us-east-2"}}
	if err := s.Sources.RegisterSource(ns, kind, service, spec); err != nil {
		t.Fatalf("register source: %v", err)
	}
}

func (s Subject) ensureSource(t *testing.T, ns uint, service string) {
	t.Helper()
	if err := s.Sources.EnsureSource(ns, service); err != nil {
		t.Fatal(err)
	}
}

func (s Subject) pendingCount(t *testing.T) int {
	t.Helper()
	var resp struct {
		Count int `json:"count"`
	}
	s.get(t, "/api/sources/pending/count", &resp)
	return resp.Count
}

// TestIngestHandler_EnrichesConfiguredService is the case the
// api-server-should-parse-like-weftd discussion was about: a service
// with a configured schema gets Level/TraceId/PayloadJson filled
// in by enrichRows before the row ever reaches the coordinator's
// IngestStream — matching what weftd does client-side and what the
// CloudWatch poller does before shard.Ingest, neither of which
// IngestStream re-parses on its own.
func ingestEnrichesConfiguredService(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)

	// level_field=1, trace_id_field=2: "ERROR req-42 db timeout" →
	// Level="ERROR", TraceId="req-42".
	schemaYAML := "app: myapp\nformat: text\nlevel_field: \"1\"\ntrace_id_field: \"2\"\n"
	s.addSource(t, ns.ID, "direct", "myapp", "", schemaYAML)

	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - ERROR req-42 db timeout`
	body := hbody(herokuFrame(msg))

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.IngestHandler(api.HerokuAdapter{}).ServeHTTP(w, req)

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
func ingestPassthroughWhenServiceNotConfigured(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, _ := s.ingestToken(t)
	// Deliberately no AddLogGroup call — "unconfigured" service.

	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host neverconfigured web.1 - - plain message`
	body := hbody(herokuFrame(msg))

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.IngestHandler(api.HerokuAdapter{}).ServeHTTP(w, req)

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

// TestIngestHandler_Heroku_SamplesSchemalessService: a raw-push service
// the coordinator auto-onboarded (configured, no schema) accumulates a
// sample from live traffic — same bootstrap weftd and kinesis get — while
// its rows keep flowing. A full sample ends the resample and surfaces the
// source for schema review.
func ingestHerokuSamplesSchemalessService(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)
	s.ensureSource(t, ns.ID, "myapp")

	postHeroku(t, srv, token, herokuLines("myapp", 3))
	waitForRows(t, fake, 3)

	lg := s.source(t, ns.Name, "myapp")
	if lg.Status != sources.Configured {
		t.Errorf("Status = %q, want configured — sampling must not gate ingest", lg.Status)
	}
	if lg.SampleCollected != 3 {
		t.Errorf("SampleCollected = %d, want 3", lg.SampleCollected)
	}
	if lg.ResampleRequestedAt == nil {
		t.Error("resample flag cleared early, want it held until the sample is full")
	}

	postHeroku(t, srv, token, herokuLines("myapp", source.SampleTarget))
	waitForRows(t, fake, 3+source.SampleTarget)

	lg = s.source(t, ns.Name, "myapp")
	if lg.SampleCollected != source.SampleTarget {
		t.Errorf("SampleCollected = %d, want %d (capped)", lg.SampleCollected, source.SampleTarget)
	}
	if lg.ResampleRequestedAt != nil {
		t.Error("resample flag still set after a full sample")
	}
	if n := s.pendingCount(t); n != 1 {
		t.Errorf("pending count = %d, want 1 — a full schemaless sample awaits review", n)
	}
}

// TestIngestHandler_Heroku_ConfiguredServiceNotSampled: a service with a
// schema and no resample request must not touch its sample.
func ingestHerokuConfiguredServiceNotSampled(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)
	s.addSource(t, ns.ID, "direct", "myapp", "", "app: myapp\nformat: text\n")

	postHeroku(t, srv, token, herokuLines("myapp", 3))
	waitForRows(t, fake, 3)

	lg := s.source(t, ns.Name, "myapp")
	if lg.SampleCollected != 0 || lg.Sample != "" {
		t.Errorf("sample = %q (%d), want none for a configured source", lg.Sample, lg.SampleCollected)
	}
}

// A brand-new push service has no log_group row when its first batch is
// ingested (the provisioner creates it after the rows are forwarded), so the
// first batch must create it rather than being dropped from the sample:
// otherwise a service that pushes exactly one sample's worth of lines in
// batches ends up with only the later batches.
func ingestHerokuFirstBatchOfNewServiceIsSampled(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)

	postHeroku(t, srv, token, herokuLines("brandnew", 25))
	waitForRows(t, fake, 25)

	lg := s.source(t, ns.Name, "brandnew")
	if lg.SampleCollected != 25 {
		t.Errorf("after the first batch SampleCollected = %d, want 25", lg.SampleCollected)
	}

	postHeroku(t, srv, token, herokuLines("brandnew", 25))
	waitForRows(t, fake, 50)
	lg = s.source(t, ns.Name, "brandnew")
	if lg.SampleCollected != source.SampleTarget {
		t.Errorf("after two batches SampleCollected = %d, want %d", lg.SampleCollected, source.SampleTarget)
	}
}

// TestIngestHandler_CloudWatch_GzipAndPlain covers both branches of the
// open "gzip-or-not" question in one request — decodeRecord must
// handle a record either way without the caller needing to know which.
func ingestCloudWatchGzipAndPlain(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)
	s.addSource(t, ns.ID, "kinesis", "myapp", "/aws/lambda/myapp", "")

	gz := gzipEnvelope(t, dataMessageEnvelope("/aws/lambda/myapp", "gzip line"))
	plain := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/myapp", "plain line"))
	body := firehoseBody("req-1", gz, plain)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	var resp struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RequestID != "req-1" {
		t.Errorf("echoed requestId = %q, want req-1", resp.RequestID)
	}

	rows := waitForRows(t, fake, 2)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	var raws []string
	for _, r := range rows {
		if r.Service != "myapp" || r.Kind != "main" {
			t.Errorf("row service/kind = %s/%s, want myapp/main", r.Service, r.Kind)
		}
		raws = append(raws, r.Raw)
	}
	if !(raws[0] == "gzip line" || raws[1] == "gzip line") || !(raws[0] == "plain line" || raws[1] == "plain line") {
		t.Errorf("Raw values = %v, want [gzip line, plain line] in some order", raws)
	}
}

// TestIngestHandler_CloudWatch_ControlMessage: Firehose's periodic
// connectivity check carries no log events and must still get a 200 +
// echoed requestId, not the default 204 (there's nothing IngestHandler
// would otherwise forward, and a non-2xx would make Firehose think the
// endpoint is unhealthy).
func ingestCloudWatchControlMessage(t *testing.T, s Subject) {
	srv := s.Server
	_, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, _ := s.ingestToken(t)

	control := plainEnvelope(t, map[string]any{"messageType": "CONTROL_MESSAGE"})
	body := firehoseBody("req-control", control)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	var resp struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RequestID != "req-control" {
		t.Errorf("echoed requestId = %q, want req-control", resp.RequestID)
	}
}

// TestIngestHandler_CloudWatch_UnknownLogGroupDropped: an event whose
// AWS logGroup has no matching log_groups row in this namespace must be
// dropped, not routed to service="" — the request still succeeds (from
// Firehose's point of view the delivery succeeded; routing is our
// problem, not something Firehose should retry over).
func ingestCloudWatchUnknownLogGroupDropped(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)
	// Configure a different log group than the one in the envelope.
	s.addSource(t, ns.ID, "kinesis", "myapp", "/aws/lambda/myapp", "")

	env := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/unregistered", "orphan line"))
	body := firehoseBody("req-2", env)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if rows, _, calls := fake.snapshot(); len(rows) != 0 || calls != 0 {
		t.Fatalf("got %d rows / %d stream calls, want 0/0 (unregistered log group should be dropped, no stream opened)", len(rows), calls)
	}
}

// TestIngestHandler_CloudWatch_Unauthorized: the shared IngestHandler
// auth path (agent-token validation) still applies — a missing/invalid
// access key must 401, same as Heroku.
func ingestCloudWatchUnauthorized(t *testing.T, s Subject) {
	srv := s.Server
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestIngestHandler_CloudWatch_AccumulatesPendingSample covers the
// kinesis-mode schema-bootstrap path: a freshly registered source (created via
// `weft source add --type kinesis`, i.e. store.RegisterSource)
// has no schema yet, so its real Firehose-pushed rows must accumulate
// into the row's Sample/Detection — the push-model equivalent of
// weftd's SubmitSample — rather than being silently dropped or ignored.
func ingestCloudWatchAccumulatesPendingSample(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)
	s.registerSource(t, ns.ID, "kinesis", "cold-shard", "/aws/lambda/weft-cold-shard-query")

	env := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/weft-cold-shard-query", "line one", "line two"))
	body := firehoseBody("req-sample", env)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	// Rows still forward to the coordinator even while pending — a
	// pending source ingests raw, unparsed rows exactly like a
	// not-yet-configured weftd source; sample accumulation is additive,
	// not a substitute for ingestion.
	waitForRows(t, fake, 2)

	lg := s.source(t, ns.Name, "cold-shard")
	if lg.Status != sources.Configured || lg.ResampleRequestedAt == nil {
		t.Errorf("Status = %q, resample requested = %v; want configured and still collecting (2 lines < SampleTarget)", lg.Status, lg.ResampleRequestedAt != nil)
	}
	if lg.SampleCollected != 2 {
		t.Errorf("SampleCollected = %d, want 2", lg.SampleCollected)
	}
	var sample []string
	if err := json.Unmarshal([]byte(lg.Sample), &sample); err != nil {
		t.Fatalf("unmarshal Sample: %v", err)
	}
	if len(sample) != 2 || sample[0] != "line one" || sample[1] != "line two" {
		t.Errorf("Sample = %v, want [line one, line two]", sample)
	}
	if lg.Detection == "" {
		t.Error("Detection is empty; accumulatePendingSamples should run ingest.Detect on the accumulated sample")
	}

	// A second batch appends rather than overwriting.
	env2 := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/weft-cold-shard-query", "line three"))
	body2 := firehoseBody("req-sample-2", env2)
	req2 := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body2))
	req2.Header.Set("X-Amz-Firehose-Access-Key", token)
	w2 := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w2.Code, w2.Body.String())
	}
	waitForRows(t, fake, 3)

	lg = s.source(t, ns.Name, "cold-shard")
	if lg.SampleCollected != 3 {
		t.Errorf("SampleCollected after second batch = %d, want 3 (appended, not overwritten)", lg.SampleCollected)
	}
}

// TestIngestHandler_CloudWatch_ConfiguredSourceNotResampled: once a
// source is StatusConfigured, ingest traffic must not touch its
// Sample/Detection at all — accumulatePendingSamples is
// bootstrap-only, not a resample mechanism (resample is its own
// explicit, human-requested flow elsewhere).
func ingestCloudWatchConfiguredSourceNotResampled(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)

	token, ns := s.ingestToken(t)
	s.addSource(t, ns.ID, "kinesis", "cold-shard", "/aws/lambda/weft-cold-shard-query", "app: cold-shard\nformat: text\n")

	env := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/weft-cold-shard-query", "already configured line"))
	body := firehoseBody("req-configured", env)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(api.CloudWatchAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	waitForRows(t, fake, 1)

	lg := s.source(t, ns.Name, "cold-shard")
	if lg.Sample != "" {
		t.Errorf("Sample = %q, want empty — a configured source must not accumulate a sample", lg.Sample)
	}
	if lg.SampleCollected != 0 {
		t.Errorf("SampleCollected = %d, want 0", lg.SampleCollected)
	}
}

// TestIngestHandler_ForwardsTraceIDToCoordinator proves the request's
// trace ID rides the IngestStream gRPC call as `weft-trace-id` metadata
// (context.Value does not cross gRPC on its own), so the coordinator's
// logs for an HTTP-pushed batch join the request's trace.
func ingestForwardsTraceID(t *testing.T, s Subject) {
	srv := s.Server
	fake, coordClient := startFakeCoord(t)
	srv.SetCoordClient(coordClient)
	token, _ := s.ingestToken(t)

	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - hello`
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(hbody(herokuFrame(msg))))
	req = req.WithContext(weftsdk.ContextWithTraceID(context.Background(), "ingest-trace-123"))
	w := httptest.NewRecorder()
	srv.IngestHandler(api.HerokuAdapter{}).ServeHTTP(w, req)
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
