//go:build !defaultshape

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// otlpFixture is a captured request from Render's metrics stream
// (api/testdata/render_otlp_metrics.jsonl): the connectivity test, a compute
// tick split across requests, a second instance, and a persistent disk.
type otlpFixtureReq struct {
	Path        string          `json:"path"`
	ContentType string          `json:"content_type"`
	Body        json.RawMessage `json:"body"`
}

func loadOTLPFixture(t *testing.T) []otlpFixtureReq {
	t.Helper()
	f, err := os.Open("testdata/render_otlp_metrics.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []otlpFixtureReq
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r otlpFixtureReq
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func parseOTLP(t *testing.T, body []byte) []*pb.LogRow {
	t.Helper()
	h := http.Header{"Content-Type": {"application/json"}}
	rows, _, err := OTLPAdapter{}.Parse(strings.NewReader(string(body)), h, AdapterContext{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return rows
}

func payloadOf(t *testing.T, r *pb.LogRow) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.PayloadJson, &m); err != nil {
		t.Fatalf("payload %q: %v", r.PayloadJson, err)
	}
	return m
}

func TestOTLPAdapter_Auth(t *testing.T) {
	a := OTLPAdapter{}
	req := httptest.NewRequest("POST", "/api/ingest/otlp/v1/metrics", nil)
	if _, err := a.Auth(req); err == nil {
		t.Error("no Authorization header: want error")
	}
	req.Header.Set("Authorization", "Basic abc")
	if _, err := a.Auth(req); err == nil {
		t.Error("non-bearer scheme: want error")
	}
	req.Header.Set("Authorization", "Bearer wft_abc")
	tok, err := a.Auth(req)
	if err != nil || tok != "wft_abc" {
		t.Errorf("Auth = %q, %v; want wft_abc", tok, err)
	}
}

// TestOTLPAdapter_ConnectivityTest: Render's integration ping carries only a
// service.name and no instance, so it yields no rows (not a fake service).
func TestOTLPAdapter_ConnectivityTest(t *testing.T) {
	fx := loadOTLPFixture(t)
	if rows := parseOTLP(t, fx[0].Body); len(rows) != 0 {
		t.Errorf("connectivity test produced %d rows, want 0", len(rows))
	}
}

// TestOTLPAdapter_ComputeTick: one resource-metrics entry becomes one row, so
// a tick split across requests is never assembled. The cpu pair is one row;
// rss, cache and each network counter are rows of their own.
func TestOTLPAdapter_ComputeTick(t *testing.T) {
	fx := loadOTLPFixture(t)
	rows := parseOTLP(t, fx[1].Body)
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5 (cpu, rss, cache, rx, tx)", len(rows))
	}
	for _, r := range rows {
		if r.Service != "render-capture-testsvc" || r.Instance != "j9fbv" {
			t.Errorf("service/instance = %q/%q", r.Service, r.Instance)
		}
		if r.Cluster != "My project" {
			t.Errorf("cluster = %q, want the service.project", r.Cluster)
		}
		if r.TsUnixNs != 1794196035000000000 {
			t.Errorf("ts = %d", r.TsUnixNs)
		}
		if r.Level != "INFO" || r.Raw != string(r.PayloadJson) {
			t.Errorf("level/raw = %q/%q", r.Level, r.Raw)
		}
	}

	byKey := map[string]*pb.LogRow{}
	for _, r := range rows {
		p := payloadOf(t, r)
		for k := range p {
			byKey[r.Kind+"/"+k] = r
		}
	}
	cpu := payloadOf(t, byKey["host/cpu"])["cpu"].(map[string]any)
	if cpu["cores"] != 0.5 || cpu["time_s"] != 831.093592 {
		t.Errorf("cpu = %v", cpu)
	}
	if len(cpu) != 2 {
		t.Errorf("cpu has extra keys: %v", cpu)
	}
	// rss and cache are separate host/memory rows.
	var mem []map[string]any
	for _, r := range rows {
		if m, ok := payloadOf(t, r)["memory"].(map[string]any); ok {
			mem = append(mem, m)
		}
	}
	if len(mem) != 2 {
		t.Fatalf("memory rows = %v, want rss and cache", mem)
	}
	got := map[string]any{}
	for _, m := range mem {
		for k, v := range m {
			got[k] = v
		}
	}
	if got["rss_mb"] != 19.34 || got["cache_mb"] != float64(0) {
		t.Errorf("memory = %v", got)
	}
	var rx, tx bool
	for _, r := range rows {
		if r.Kind != "network" {
			continue
		}
		p := payloadOf(t, r)
		if v, ok := p["rx_bytes_total"]; ok && v == float64(299548311) {
			rx = true
		}
		if v, ok := p["tx_bytes_total"]; ok && v == float64(161801597) {
			tx = true
		}
		if _, hasIface := p["iface"]; hasIface {
			t.Errorf("network row has an iface: %v", p)
		}
	}
	if !rx || !tx {
		t.Errorf("network totals missing: rx=%v tx=%v", rx, tx)
	}
}

// TestOTLPAdapter_MemoryPair: usage and limit arrive together and give pct.
func TestOTLPAdapter_MemoryPair(t *testing.T) {
	fx := loadOTLPFixture(t)
	rows := parseOTLP(t, fx[2].Body)
	if len(rows) != 1 || rows[0].Kind != "host" {
		t.Fatalf("rows = %v", rows)
	}
	mem := payloadOf(t, rows[0])["memory"].(map[string]any)
	want := map[string]any{"used_mb": 30.32, "total_mb": float64(512), "pct": 5.92}
	for k, v := range want {
		if mem[k] != v {
			t.Errorf("memory.%s = %v, want %v", k, mem[k], v)
		}
	}
}

// TestOTLPAdapter_Disk: the disk arrives as its own resource, with the disk id
// as its instance and no mount path. It splits to the id's suffix, and the
// service is the owning service's. Capacity and usage come in separate
// entries, so they are separate rows and there is no pct.
func TestOTLPAdapter_Disk(t *testing.T) {
	fx := loadOTLPFixture(t)
	var disk []*pb.LogRow
	for _, r := range parseOTLP(t, fx[3].Body) {
		if r.Kind == "disk" {
			disk = append(disk, r)
		}
	}
	if len(disk) != 2 {
		t.Fatalf("disk rows = %d, want 2 (capacity, usage)", len(disk))
	}
	got := map[string]any{}
	for _, r := range disk {
		if r.Service != "render-capture-testsvc" || r.Instance != "dbokb5ou01pc73fhr4ag" {
			t.Errorf("service/instance = %q/%q", r.Service, r.Instance)
		}
		for k, v := range payloadOf(t, r) {
			got[k] = v
		}
	}
	if got["total_gb"] != 9.75 || got["used_gb"] != float64(0) {
		t.Errorf("disk fields = %v", got)
	}
	for _, k := range []string{"path", "pct"} {
		if _, ok := got[k]; ok {
			t.Errorf("disk rows carry %q: %v", k, got)
		}
	}
}

// TestOTLPAdapter_DropsUnsupported: metrics outside the kept set (HTTP here,
// present in the capture) produce no rows and no error.
func TestOTLPAdapter_DropsUnsupported(t *testing.T) {
	fx := loadOTLPFixture(t)
	var sawHTTP bool
	for _, f := range fx {
		if strings.Contains(string(f.Body), "render.service.http.requests.total") {
			sawHTTP = true
		}
		for _, r := range parseOTLP(t, f.Body) {
			if strings.Contains(r.Raw, "requests") || strings.Contains(r.Raw, "status_code") {
				t.Errorf("row carries an HTTP metric: %s", r.Raw)
			}
		}
	}
	if !sawHTTP {
		t.Fatal("fixture no longer contains an HTTP metric; the test proves nothing")
	}
}

func TestOTLPAdapter_RejectsNonJSON(t *testing.T) {
	h := http.Header{"Content-Type": {"application/x-protobuf"}}
	if _, _, err := (OTLPAdapter{}).Parse(strings.NewReader("\x0a\x00"), h, AdapterContext{}); err == nil {
		t.Error("protobuf body: want an error saying only JSON is supported")
	}
}

func TestOTLPAdapter_WriteSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	OTLPAdapter{}.WriteSuccess(w, nil)
	if w.Code != 200 || w.Body.String() != "{}" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("response = %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
}

func postOTLP(t *testing.T, srv *Server, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/otlp/v1/metrics", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.IngestHandler(OTLPAdapter{}).ServeHTTP(w, req)
	return w
}

// TestIngestHandler_OTLP_KeepsPayloadOfConfiguredService: the service already
// has a log schema (Render syslog onboarded it). Enrichment resolves schemas
// by service alone, so without a guard it would re-parse a metric row's JSON
// as a log line and overwrite its payload, level and timestamp.
func TestIngestHandler_OTLP_KeepsPayloadOfConfiguredService(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient
	token, ns := setupIngestOrg(t, s)

	schemaYAML := "app: render-capture-testsvc\nformat: text\nlevel_field: \"1\"\ntrace_id_field: \"2\"\n"
	if err := s.AddLogGroup(context.Background(), ns.ID, store.SourceTypeDirect, "render-capture-testsvc", "", "us-east-2", schemaYAML); err != nil {
		t.Fatal(err)
	}

	fx := loadOTLPFixture(t)
	w := postOTLP(t, srv, token, fx[1].Body)
	if w.Code != http.StatusOK || w.Body.String() != "{}" {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	rows := waitForRows(t, fake, 5)
	for _, r := range rows {
		if r.Level != "INFO" {
			t.Errorf("Level = %q, want INFO: the log schema was applied to a metric row", r.Level)
		}
		if r.TsUnixNs != 1794196035000000000 {
			t.Errorf("ts = %d: the log schema overwrote the sample time", r.TsUnixNs)
		}
		if len(r.PayloadJson) == 0 || r.PayloadJson[0] != '{' || !strings.Contains(string(r.PayloadJson), `"`) {
			t.Errorf("payload = %q", r.PayloadJson)
		}
		if string(r.PayloadJson) != r.Raw {
			t.Errorf("payload %q differs from raw %q: the log schema re-parsed a metric row", r.PayloadJson, r.Raw)
		}
	}
}

// TestIngestHandler_OTLP_NotSampledAsLogs: a service collecting a log sample
// for schema detection must not receive metric rows in it.
func TestIngestHandler_OTLP_NotSampledAsLogs(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient
	token, ns := setupIngestOrg(t, s)
	if err := s.EnsureServiceLogGroup(ns.ID, "render-capture-testsvc"); err != nil {
		t.Fatal(err)
	}

	fx := loadOTLPFixture(t)
	if w := postOTLP(t, srv, token, fx[1].Body); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	waitForRows(t, fake, 5)

	lg, err := s.FindLogGroup(ns.OrgID, ns.Name, "render-capture-testsvc")
	if err != nil || lg == nil {
		t.Fatalf("FindLogGroup = %v, %v", lg, err)
	}
	if lg.SampleCollected != 0 {
		t.Errorf("SampleCollected = %d, want 0: metric rows were added to the log sample", lg.SampleCollected)
	}
}
