//go:build !defaultshape

package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// firehoseBody builds a Firehose HTTP-endpoint-delivery request body
// from a set of already-encoded (base64, maybe-gzipped) CloudWatch
// envelope records.
func firehoseBody(requestID string, records ...string) string {
	b, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"timestamp": 1750000000000,
		"records": func() []map[string]string {
			out := make([]map[string]string, len(records))
			for i, r := range records {
				out[i] = map[string]string{"data": r}
			}
			return out
		}(),
	})
	return string(b)
}

// gzipEnvelope base64-encodes gzip-compressed envelope JSON — the
// "still gzipped" branch of the open gzip-or-not question.
func gzipEnvelope(t *testing.T, envelope map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// plainEnvelope base64-encodes uncompressed envelope JSON — the "plain
// JSON" branch.
func plainEnvelope(t *testing.T, envelope map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func dataMessageEnvelope(logGroup string, messages ...string) map[string]any {
	events := make([]map[string]any, len(messages))
	for i, m := range messages {
		events[i] = map[string]any{"id": "evt", "timestamp": 1750000000000, "message": m}
	}
	return map[string]any{
		"messageType": "DATA_MESSAGE",
		"owner":       "123456789012",
		"logGroup":    logGroup,
		"logStream":   "stream1",
		"logEvents":   events,
	}
}

// TestCloudWatchAdapter_Auth_HeaderKey: X-Amz-Firehose-Access-Key is the
// bearer-equivalent, per Firehose's HTTP endpoint delivery spec.
func TestCloudWatchAdapter_Auth_HeaderKey(t *testing.T) {
	a := CloudWatchAdapter{}
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", nil)
	req.Header.Set("X-Amz-Firehose-Access-Key", "wft_abcdef")
	got, err := a.Auth(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wft_abcdef" {
		t.Errorf("token = %q, want wft_abcdef", got)
	}
}

func TestCloudWatchAdapter_Auth_Missing(t *testing.T) {
	a := CloudWatchAdapter{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, err := a.Auth(req); err == nil {
		t.Error("expected error for missing access key header")
	}
}

// TestIngestHandler_CloudWatch_GzipAndPlain covers both branches of the
// open "gzip-or-not" question in one request — decodeRecord must
// handle a record either way without the caller needing to know which.
func TestIngestHandler_CloudWatch_GzipAndPlain(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)
	if err := s.AddLogGroup(context.Background(), ns.ID, store.SourceTypeKinesis, "myapp", "/aws/lambda/myapp", "us-east-2", ""); err != nil {
		t.Fatalf("add log group: %v", err)
	}

	gz := gzipEnvelope(t, dataMessageEnvelope("/aws/lambda/myapp", "gzip line"))
	plain := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/myapp", "plain line"))
	body := firehoseBody("req-1", gz, plain)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)

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
func TestIngestHandler_CloudWatch_ControlMessage(t *testing.T) {
	srv, s := testServer(t)
	_, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, _ := setupIngestOrg(t, s)

	control := plainEnvelope(t, map[string]any{"messageType": "CONTROL_MESSAGE"})
	body := firehoseBody("req-control", control)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)

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
func TestIngestHandler_CloudWatch_UnknownLogGroupDropped(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)
	// Configure a different log group than the one in the envelope.
	if err := s.AddLogGroup(context.Background(), ns.ID, store.SourceTypeKinesis, "myapp", "/aws/lambda/myapp", "us-east-2", ""); err != nil {
		t.Fatalf("add log group: %v", err)
	}

	env := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/unregistered", "orphan line"))
	body := firehoseBody("req-2", env)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)

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
func TestIngestHandler_CloudWatch_Unauthorized(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
