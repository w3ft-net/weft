package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/ingest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// Schema runs the routes a wizard drives to give a source its schema: probing a
// sample for a field, previewing a schema on it, and reading a configured
// source's schema with a live preview of its latest lines.
func Schema(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"Probe", probeSource},
		{"ProbeModelFallback", probeModelFallback},
		{"ProbeDetectedCandidatesByIndex", probeDetectedCandidates},
		{"PreviewMarksFieldsOnAPendingSample", previewMarksFields},
		{"PreviewWithNoSampleIs409", previewNoSample},
		{"PreviewOfAnotherGroupsSourceIs404", previewOtherGroup},
		{"PreviewRequiresAuth", previewRequiresAuth},
		{"SourceSchemaIncludesALivePreview", schemaIncludesLivePreview},
		{"SourceSchemaSurvivesAnEmptyCoordinator", schemaSurvivesCoordinatorFailure(func() *previewCoord { return &previewCoord{} })},
		{"SourceSchemaSurvivesAFailingCoordinator", schemaSurvivesCoordinatorFailure(func() *previewCoord {
			return &previewCoord{err: status.Error(codes.Unavailable, "down")}
		})},
		{"SourceSchemaSDKNote", schemaSDKNote},
		{"PreviewForwardsTheCallersTraceID", schemaForwardsTraceID},
	})
}

var previewSchemaBody = map[string]any{
	"format":           "text",
	"timestamp_field":  "1",
	"timestamp_format": "2006-01-02T15:04:05Z07:00",
	"timestamp_fields": 1,
	"level_field":      "2",
	"trace_id_field":   "none",
	"trace_id_pattern": `id=(\w+)`,
}

const previewSampleJSON = `["2026-10-16T18:18:55.649Z INFO request id=abc123 dur=8ms", "2026-10-16T18:18:56.100Z WARN request id=def456 dur=442ms"]`

// previewCoord answers Execute with canned lines (or an error), recording the
// request and the trace ID metadata it arrived with.
type previewCoord struct {
	pb.UnimplementedWeftCoordinatorServer
	mu      sync.Mutex
	lines   []string
	err     error
	reqs    []*pb.CoordinatorExecuteRequest
	traceID []string
}

func (f *previewCoord) Execute(req *pb.CoordinatorExecuteRequest, stream grpc.ServerStreamingServer[pb.CoordinatorExecuteResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.traceID = append(f.traceID, md.Get("weft-trace-id")...)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	var ls []*pb.Line
	for _, l := range f.lines {
		ls = append(ls, &pb.Line{Raw: l})
	}
	return stream.Send(&pb.CoordinatorExecuteResponse{Lines: ls})
}

func startPreviewCoord(t *testing.T, f *previewCoord) pb.WeftCoordinatorClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(g, f)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(weftsdk.GRPC.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(weftsdk.GRPC.StreamClientInterceptor()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewWeftCoordinatorClient(conn)
}

// submit leaves a source in namespace ns1 with the sample.
func (s Subject) submit(t *testing.T, service, sample, detection string, collected int) uint {
	t.Helper()
	ns := s.namespace(t, "ns1")
	if err := s.Sources.SubmitSample(ns, service, source.SampleSubmission{
		Instance: "i", Deployment: "d", SampleJSON: sample, DetectionJSON: detection, Collected: collected,
	}); err != nil {
		t.Fatal(err)
	}
	return ns
}

func (s Subject) postBody(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	w := s.post(t, path, body)
	return w.Code, w.Body.Bytes()
}

// The probe route extracts from the source's stored sample, so the wizard never
// ships sample lines back to the server, and reports the result for every line
// plus the schema fields to merge into the configure form.
func probeSource(t *testing.T, s Subject) {
	sample, _ := json.Marshal([]string{
		"2026-10-15 12:00:00.100 DEBUG handled GET /healthz 200 1ms",
		"2026-10-15 12:00:00.200 INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a871",
		"2026-10-15 12:00:00.300 INFO handled GET /api/orders 500 90ms trace_id=aa01bb02cc03dd04",
	})
	s.submit(t, "api", string(sample), `{}`, 3)

	code, out := s.postBody(t, "/api/sources/ns1/api/probe", map[string]any{
		"field": "trace_id",
		"base":  map[string]any{"format": "text"},
		"marks": []map[string]any{{
			"line": 1, "prefix": "2026-10-15 12:00:00.200 INFO handled GET /api/items 200 12ms trace_id=", "text": "3f9a1c0de4b2a871",
		}},
	})
	if code != 200 {
		t.Fatalf("probe = %d: %s", code, out)
	}
	var resp struct {
		Extractor   map[string]any   `json:"extractor"`
		Results     []map[string]any `json:"results"`
		Matched     int              `json:"matched"`
		Total       int              `json:"total"`
		SchemaPatch map[string]any   `json:"schema_patch"`
		Description string           `json:"description"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if resp.Matched != 2 || resp.Total != 3 {
		t.Errorf("matched %d/%d, want 2/3: %s", resp.Matched, resp.Total, out)
	}
	if resp.Results[2]["value"] != "aa01bb02cc03dd04" {
		t.Errorf("line 2 value = %v", resp.Results[2]["value"])
	}
	if !strings.Contains(resp.Description, `without the leading "trace_id="`) {
		t.Errorf("description = %q, want the rule in words", resp.Description)
	}
	if resp.SchemaPatch["trace_id_field"] != "9" || resp.SchemaPatch["trace_id_pattern"] != "" || resp.SchemaPatch["extract"] == nil {
		t.Errorf("schema_patch = %v, want field 9 with an extract spec and no pattern", resp.SchemaPatch)
	}
	if code, _ := s.postBody(t, "/api/sources/ns1/api/probe", map[string]any{"field": "nope", "marks": []any{}}); code != 400 {
		t.Errorf("unknown field = %d, want 400", code)
	}
	if code, _ := s.postBody(t, "/api/sources/ns1/missing/probe", map[string]any{"field": "trace_id"}); code != 404 {
		t.Errorf("unknown source = %d, want 404", code)
	}
}

// proposingClassifier is a classifier that can also propose extractors.
type proposingClassifier struct{ cands []ingest.ProbeExtractor }

func (proposingClassifier) Classify(context.Context, *ingest.Detection) (*ingest.ClassifierResult, error) {
	return &ingest.ClassifierResult{}, nil
}
func (p proposingClassifier) ProposeExtractors(context.Context, ingest.ProposeInput) ([]ingest.ProbeExtractor, error) {
	return p.cands, nil
}

// When the built-in search finds no rule, the server asks its classifier (if it
// can propose) and reports where the rule came from; the proposal is still
// verified by the parser before it is returned.
func probeModelFallback(t *testing.T, s Subject) {
	s.Server.SetClassifier(proposingClassifier{cands: []ingest.ProbeExtractor{{Field: "none", Pattern: `(?i)id[=:]\s*(\w+)`}}})
	sample, _ := json.Marshal([]string{"handled id=ab12 ok", "handled ID: cd34 ok", "handled nothing here"})
	s.submit(t, "api", string(sample), `{}`, 3)

	code, out := s.postBody(t, "/api/sources/ns1/api/probe", map[string]any{
		"field": "trace_id",
		"base":  map[string]any{"format": "text"},
		"marks": []map[string]any{
			{"line": 0, "prefix": "handled id=", "text": "ab12"},
			{"line": 1, "prefix": "handled ID: ", "text": "cd34"},
		},
	})
	if code != 200 {
		t.Fatalf("probe = %d: %s", code, out)
	}
	var resp struct {
		Source  string `json:"source"`
		Matched int    `json:"matched"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Source != "model" || resp.Matched != 2 {
		t.Errorf("source=%q matched=%d, want model and 2: %s", resp.Source, resp.Matched, out)
	}
}

// Every detected candidate can be asked for by index, and the response says how
// many there are, so a client can offer them one after another.
func probeDetectedCandidates(t *testing.T, s Subject) {
	var lines []string
	for i := 0; i < 8; i++ {
		lines = append(lines, fmt.Sprintf("2026-10-15 12:00:%02d %s %s /p%d", i, []string{"INFO", "WARN"}[i%2], []string{"GET", "POST"}[i%2], i))
	}
	sample, _ := json.Marshal(lines)
	det := ingest.Detect(lines)
	detJSON, _ := json.Marshal(det)
	if len(det.LevelCandidates) != 2 {
		t.Fatalf("setup: want two categorical candidates, got %+v", det.LevelCandidates)
	}
	s.submit(t, "api", string(sample), string(detJSON), len(lines))

	type fields struct {
		Extractor *struct {
			Field string `json:"field"`
		} `json:"extractor"`
		DetectedTotal int `json:"detected_total"`
	}
	probe := func(idx int) (f fields) {
		code, out := s.postBody(t, "/api/sources/ns1/api/probe", map[string]any{
			"field": "level", "base": map[string]any{"format": "text"},
			"marks": []any{}, "use_detected": true, "detected_index": idx,
		})
		if code != 200 {
			t.Fatalf("probe = %d: %s", code, out)
		}
		if err := json.Unmarshal(out, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	first, second, beyond := probe(0), probe(1), probe(2)
	if first.DetectedTotal != 2 || first.Extractor == nil || first.Extractor.Field != "3" {
		t.Errorf("candidate 0 = %+v, want field 3 of 2", first)
	}
	if second.Extractor == nil || second.Extractor.Field != "4" {
		t.Errorf("candidate 1 = %+v, want field 4", second)
	}
	if beyond.Extractor != nil || beyond.DetectedTotal != 2 {
		t.Errorf("candidate 2 = %+v, want none, total still 2", beyond)
	}
}

func previewMarksFields(t *testing.T, s Subject) {
	s.submit(t, "worker", previewSampleJSON, `{}`, 2)
	w := s.post(t, "/api/sources/ns1/worker/schema-preview", previewSchemaBody)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var p ingest.Preview
	decode(t, w, &p)
	if len(p.Lines) != 2 {
		t.Fatalf("lines = %d", len(p.Lines))
	}
	got := map[ingest.ProbeField]string{}
	for _, seg := range p.Lines[0] {
		if seg.Field != "" {
			got[seg.Field] = seg.Text
		}
	}
	want := map[ingest.ProbeField]string{
		ingest.ProbeTimestamp: "2026-10-16T18:18:55.649Z",
		ingest.ProbeLevel:     "INFO",
		ingest.ProbeTraceID:   "abc123",
	}
	for f, v := range want {
		if got[f] != v {
			t.Errorf("field %s = %q, want %q (all: %v)", f, got[f], v, got)
		}
	}
	// Previewing never mutates: the source is still pending.
	if st := s.source(t, "ns1", "worker").Status; st != "pending" {
		t.Errorf("status = %q, want pending", st)
	}
}

func previewNoSample(t *testing.T, s Subject) {
	s.submit(t, "worker", `[]`, `{}`, 0)
	w := s.post(t, "/api/sources/ns1/worker/schema-preview", previewSchemaBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d %s, want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sample") {
		t.Errorf("message should mention the sample: %s", w.Body.String())
	}
}

func previewOtherGroup(t *testing.T, s Subject) {
	s.needsOther(t)
	s.submit(t, "worker", previewSampleJSON, `{}`, 2)
	b, _ := json.Marshal(previewSchemaBody)
	w := s.DoOther("POST", "/api/sources/ns1/worker/schema-preview", b)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d %s, want 404", w.Code, w.Body.String())
	}
}

func previewRequiresAuth(t *testing.T, s Subject) {
	b, _ := json.Marshal(previewSchemaBody)
	if w := s.Anon("POST", "/api/sources/ns1/worker/schema-preview", b); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
}

// configuredPreviewSource configures a source through the real route so the
// stored schema is what production would hold.
func (s Subject) configuredPreviewSource(t *testing.T) {
	t.Helper()
	s.submit(t, "worker", previewSampleJSON, `{}`, 2)
	if w := s.post(t, "/api/sources/ns1/worker/configure", previewSchemaBody); w.Code != 200 {
		t.Fatalf("configure %d %s", w.Code, w.Body.String())
	}
}

func schemaIncludesLivePreview(t *testing.T, s Subject) {
	s.configuredPreviewSource(t)
	fake := &previewCoord{lines: []string{"2026-10-16T19:00:00.000Z ERROR boom id=zzz999"}}
	s.Server.SetCoordClient(startPreviewCoord(t, fake))

	var resp struct {
		SchemaYAML string          `json:"schema_yaml"`
		Preview    *ingest.Preview `json:"preview"`
		Note       string          `json:"preview_note"`
	}
	s.get(t, "/api/sources/ns1/worker/schema", &resp)
	if resp.SchemaYAML == "" {
		t.Error("schema_yaml dropped")
	}
	if resp.Preview == nil || len(resp.Preview.Lines) != 1 {
		t.Fatalf("preview = %+v note=%q", resp.Preview, resp.Note)
	}
	found := false
	for _, seg := range resp.Preview.Lines[0] {
		if seg.Field == ingest.ProbeLevel && seg.Text == "ERROR" {
			found = true
		}
	}
	if !found {
		t.Errorf("level not marked: %+v", resp.Preview.Lines[0])
	}
	if len(fake.reqs) != 1 || fake.reqs[0].Pipeline != "tail -n 12 /log/ns1/worker/main.log" {
		t.Errorf("coordinator request = %+v", fake.reqs)
	}
}

// A coordinator that has nothing or fails costs the schema its preview and
// nothing else.
func schemaSurvivesCoordinatorFailure(fake func() *previewCoord) func(*testing.T, Subject) {
	return func(t *testing.T, s Subject) {
		s.configuredPreviewSource(t)
		s.Server.SetCoordClient(startPreviewCoord(t, fake()))
		var resp map[string]any
		s.get(t, "/api/sources/ns1/worker/schema", &resp)
		if _, ok := resp["preview"]; ok {
			t.Error("preview present, want omitted")
		}
		if resp["preview_note"] == "" || resp["preview_note"] == nil {
			t.Error("preview_note missing")
		}
		if resp["schema_yaml"] == nil {
			t.Error("schema_yaml dropped")
		}
	}
}

// A source written by the weft SDK says so on its schema: its values come from
// the line header, and a source on an ordinary schema carries no note.
func schemaSDKNote(t *testing.T, s Subject) {
	ns := s.namespace(t, "ns1")
	s.Server.SetCoordClient(startPreviewCoord(t, &previewCoord{}))
	for _, tc := range []struct {
		service string
		sdk     bool
	}{{"sdkapp", true}, {"plainapp", false}} {
		if err := s.Sources.SubmitSample(ns, tc.service, source.SampleSubmission{Instance: "i", Deployment: "d", SampleJSON: previewSampleJSON, DetectionJSON: `{}`, Collected: 2}); err != nil {
			t.Fatal(err)
		}
		yaml := "app: " + tc.service + "\nenvelope: none\nformat: text\ntimestamp_field: none\n"
		if tc.sdk {
			var err error
			if yaml, err = ingest.SDKSchema(tc.service, "json"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Sources.ConfigureSource(context.Background(), ns, tc.service, yaml); err != nil {
			t.Fatal(err)
		}
		var resp struct {
			SDKNote string `json:"sdk_note"`
		}
		s.get(t, "/api/sources/ns1/"+tc.service+"/schema", &resp)
		if tc.sdk && resp.SDKNote != ingest.SDKDerivedNote {
			t.Errorf("%s: sdk_note = %q, want the derived-values note", tc.service, resp.SDKNote)
		}
		if !tc.sdk && resp.SDKNote != "" {
			t.Errorf("%s: sdk_note = %q on an ordinary schema, want none", tc.service, resp.SDKNote)
		}
	}
}

// The live preview asks the coordinator for the source's latest lines on the
// caller's behalf, so the trace the request arrived on is the one the coordinator
// sees: context does not cross a gRPC call by itself, so this is the thing that
// could silently be wrong.
func schemaForwardsTraceID(t *testing.T, s Subject) {
	s.configuredPreviewSource(t)
	fake := &previewCoord{lines: []string{"x"}}
	s.Server.SetCoordClient(startPreviewCoord(t, fake))

	const trace = "0af7651916cd43dd8448eb211c80319c"
	s.Header.Set("traceparent", "00-"+trace+"-b7ad6b7169203331-01")
	var resp map[string]any
	s.get(t, "/api/sources/ns1/worker/schema", &resp)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.traceID) == 0 || fake.traceID[0] != trace {
		t.Fatalf("weft-trace-id seen by the coordinator = %v, want %s", fake.traceID, trace)
	}
}
