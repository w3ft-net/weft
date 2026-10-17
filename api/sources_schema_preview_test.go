package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/ingest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
	weftsdk "github.com/w3ft-net/weft-go"
)

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

// previewCoord answers Execute with canned lines (or an error), recording
// the request and the trace ID metadata it arrived with.
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
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewWeftCoordinatorClient(conn)
}

func previewUser(t *testing.T, s *store.Store, name string) (*store.Organization, string) {
	t.Helper()
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("%s-%s", name, t.Name()))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("%s-%s@example.com", name, strings.ToLower(t.Name())))
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")
	return cust, jwt
}

func doJSON(h http.Handler, method, path, jwt string, body any) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSchemaPreview_MarksFieldsOnPendingSample(t *testing.T) {
	srv, s := testServer(t)
	cust, jwt := previewUser(t, s, "prev")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	if err := s.UpsertPendingLogGroup(ns.ID, "worker", "i", "d", previewSampleJSON, `{}`, 2, false); err != nil {
		t.Fatal(err)
	}

	w := doJSON(srv.Handler(), "POST", "/api/sources/ns1/worker/schema-preview", jwt, previewSchemaBody)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var p ingest.Preview
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
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

	// Never mutates: still pending.
	lg, _ := s.FindLogGroup(cust.ID, "ns1", "worker")
	if lg.Status != store.StatusPending {
		t.Errorf("status = %q, want pending", lg.Status)
	}
}

func TestSchemaPreview_NoSampleIs409(t *testing.T) {
	srv, s := testServer(t)
	cust, jwt := previewUser(t, s, "prevnone")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	if err := s.UpsertPendingLogGroup(ns.ID, "worker", "i", "d", `[]`, `{}`, 0, false); err != nil {
		t.Fatal(err)
	}
	w := doJSON(srv.Handler(), "POST", "/api/sources/ns1/worker/schema-preview", jwt, previewSchemaBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d %s, want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sample") {
		t.Errorf("message should mention the sample: %s", w.Body.String())
	}
}

func TestSchemaPreview_OtherOrgCannotPreview(t *testing.T) {
	srv, s := testServer(t)
	cust, _ := previewUser(t, s, "prevowner")
	_, otherJWT := previewUser(t, s, "prevother")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	if err := s.UpsertPendingLogGroup(ns.ID, "worker", "i", "d", previewSampleJSON, `{}`, 2, false); err != nil {
		t.Fatal(err)
	}
	w := doJSON(srv.Handler(), "POST", "/api/sources/ns1/worker/schema-preview", otherJWT, previewSchemaBody)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d %s, want 404", w.Code, w.Body.String())
	}
}

func TestSchemaPreview_RequiresAuth(t *testing.T) {
	srv, _ := testServer(t)
	w := doJSON(srv.Handler(), "POST", "/api/sources/ns1/worker/schema-preview", "", previewSchemaBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
}

// configuredPreviewSource configures a source through the real endpoint so
// the stored schema_yaml is what production would hold.
func configuredPreviewSource(t *testing.T, srv *Server, s *store.Store) string {
	t.Helper()
	cust, jwt := previewUser(t, s, "prevget")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	if err := s.UpsertPendingLogGroup(ns.ID, "worker", "i", "d", previewSampleJSON, `{}`, 2, false); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(srv.Handler(), "POST", "/api/sources/ns1/worker/configure", jwt, previewSchemaBody); w.Code != 200 {
		t.Fatalf("configure %d %s", w.Code, w.Body.String())
	}
	return jwt
}

func TestSourceSchema_IncludesLivePreview(t *testing.T) {
	srv, s := testServer(t)
	jwt := configuredPreviewSource(t, srv, s)
	fake := &previewCoord{lines: []string{"2026-10-16T19:00:00.000Z ERROR boom id=zzz999"}}
	srv.coordClient = startPreviewCoord(t, fake)

	w := doJSON(srv.Handler(), "GET", "/api/sources/ns1/worker/schema", jwt, nil)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		SchemaYAML string          `json:"schema_yaml"`
		Preview    *ingest.Preview `json:"preview"`
		Note       string          `json:"preview_note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
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

func TestSourceSchema_SurvivesCoordinatorFailure(t *testing.T) {
	for name, fake := range map[string]*previewCoord{
		"empty": {},
		"error": {err: status.Error(codes.Unavailable, "down")},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			jwt := configuredPreviewSource(t, srv, s)
			srv.coordClient = startPreviewCoord(t, fake)
			w := doJSON(srv.Handler(), "GET", "/api/sources/ns1/worker/schema", jwt, nil)
			if w.Code != 200 {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			var resp map[string]any
			json.Unmarshal(w.Body.Bytes(), &resp)
			if _, ok := resp["preview"]; ok {
				t.Error("preview present, want omitted")
			}
			if resp["preview_note"] == "" || resp["preview_note"] == nil {
				t.Error("preview_note missing")
			}
			if resp["schema_yaml"] == nil {
				t.Error("schema_yaml dropped")
			}
		})
	}
}

func TestSourceSchema_PreviewForwardsTraceID(t *testing.T) {
	srv, s := testServer(t)
	configuredPreviewSource(t, srv, s)
	fake := &previewCoord{lines: []string{"x"}}
	srv.coordClient = startPreviewCoord(t, fake)

	cust, err := s.FindOrCreateOrg("prevget-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	// Call the handler directly: the access middleware mints its own trace
	// ID, so going through the router would hide the one under test.
	req := httptest.NewRequest("GET", "/api/sources/ns1/worker/schema", nil)
	req.SetPathValue("namespace", "ns1")
	req.SetPathValue("app", "worker")
	ctx := weftsdk.ContextWithTraceID(context.Background(), "preview-trace-1")
	ctx = auth.WithSession(ctx, &auth.Session{UserID: "u1", ActiveOrgID: cust.ID})
	w := httptest.NewRecorder()
	srv.handleSourceSchema(w, req.WithContext(ctx))
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.traceID) == 0 || fake.traceID[0] != "preview-trace-1" {
		t.Fatalf("weft-trace-id seen = %v", fake.traceID)
	}
}
