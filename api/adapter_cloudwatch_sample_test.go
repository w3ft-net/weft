package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// TestIngestHandler_CloudWatch_AccumulatesPendingSample covers the
// kinesis-mode schema-bootstrap path: a pending source (created via
// `weft source add --type kinesis`, i.e. store.CreatePendingKinesisSource)
// has no schema yet, so its real Firehose-pushed rows must accumulate
// into the row's Sample/Detection — the push-model equivalent of
// weftd's SubmitSample — rather than being silently dropped or ignored.
func TestIngestHandler_CloudWatch_AccumulatesPendingSample(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)
	if err := s.CreatePendingKinesisSource(ns.ID, "cold-shard", "/aws/lambda/weft-cold-shard-query", "us-east-2"); err != nil {
		t.Fatalf("CreatePendingKinesisSource: %v", err)
	}

	env := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/weft-cold-shard-query", "line one", "line two"))
	body := firehoseBody("req-sample", env)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	// Rows still forward to the coordinator even while pending — a
	// pending source ingests raw, unparsed rows exactly like a
	// not-yet-configured weftd source; sample accumulation is additive,
	// not a substitute for ingestion.
	waitForRows(t, fake, 2)

	orgID := ns.OrgID
	lg, err := s.FindLogGroup(orgID, ns.Name, "cold-shard")
	if err != nil {
		t.Fatalf("FindLogGroup: %v", err)
	}
	if lg == nil {
		t.Fatal("expected the pending log group row to still exist")
	}
	if lg.Status != store.StatusSampling {
		t.Errorf("Status = %q, want %q (2 lines < SampleTarget, still accumulating)", lg.Status, store.StatusSampling)
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
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w2.Code, w2.Body.String())
	}
	waitForRows(t, fake, 3)

	lg, err = s.FindLogGroup(orgID, ns.Name, "cold-shard")
	if err != nil {
		t.Fatal(err)
	}
	if lg.SampleCollected != 3 {
		t.Errorf("SampleCollected after second batch = %d, want 3 (appended, not overwritten)", lg.SampleCollected)
	}
}

// TestIngestHandler_CloudWatch_ConfiguredSourceNotResampled: once a
// source is StatusConfigured, ingest traffic must not touch its
// Sample/Detection at all — accumulatePendingSamples is
// bootstrap-only, not a resample mechanism (resample is its own
// explicit, human-requested flow elsewhere).
func TestIngestHandler_CloudWatch_ConfiguredSourceNotResampled(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	token, ns := setupIngestOrg(t, s)
	if err := s.AddLogGroup(context.Background(), ns.ID, store.SourceTypeKinesis, "cold-shard", "/aws/lambda/weft-cold-shard-query", "us-east-2", "app: cold-shard\nformat: text\n"); err != nil {
		t.Fatalf("AddLogGroup: %v", err)
	}

	env := plainEnvelope(t, dataMessageEnvelope("/aws/lambda/weft-cold-shard-query", "already configured line"))
	body := firehoseBody("req-configured", env)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", strings.NewReader(body))
	req.Header.Set("X-Amz-Firehose-Access-Key", token)
	w := httptest.NewRecorder()
	srv.IngestHandler(CloudWatchAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	waitForRows(t, fake, 1)

	lg, err := s.FindLogGroup(ns.OrgID, ns.Name, "cold-shard")
	if err != nil {
		t.Fatal(err)
	}
	if lg.Sample != "" {
		t.Errorf("Sample = %q, want empty — a configured source must not accumulate a sample", lg.Sample)
	}
	if lg.SampleCollected != 0 {
		t.Errorf("SampleCollected = %d, want 0", lg.SampleCollected)
	}
}
