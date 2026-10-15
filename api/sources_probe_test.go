package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/ingest"
)

// The probe endpoint extracts from the source's stored sample, so the wizard
// never ships sample lines back to the server, and reports the result for
// every line plus the schema fields to merge into the configure form.
func TestProbeSource(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("probe-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	sample, _ := json.Marshal([]string{
		"2026-10-15 12:00:00.100 DEBUG handled GET /healthz 200 1ms",
		"2026-10-15 12:00:00.200 INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a871",
		"2026-10-15 12:00:00.300 INFO handled GET /api/orders 500 90ms trace_id=aa01bb02cc03dd04",
	})
	if err := s.UpsertPendingLogGroup(ns.ID, "api", "i", "d", string(sample), `{}`, 3, false); err != nil {
		t.Fatal(err)
	}

	post := func(path string, body any) (int, []byte) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	code, out := post("/api/sources/ns1/api/probe", map[string]any{
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

	if code, _ := post("/api/sources/ns1/api/probe", map[string]any{"field": "nope", "marks": []any{}}); code != 400 {
		t.Errorf("unknown field = %d, want 400", code)
	}
	if code, _ := post("/api/sources/ns1/missing/probe", map[string]any{"field": "trace_id"}); code != 404 {
		t.Errorf("unknown source = %d, want 404", code)
	}
}

// proposingClassifier is a classifier that can also propose extractors.
type proposingClassifier struct{ cands []ingest.ProbeExtractor }

func (proposingClassifier) Classify(ctx context.Context, det *ingest.Detection) (*ingest.ClassifierResult, error) {
	return &ingest.ClassifierResult{}, nil
}
func (p proposingClassifier) ProposeExtractors(ctx context.Context, in ingest.ProposeInput) ([]ingest.ProbeExtractor, error) {
	return p.cands, nil
}

// When the built-in search finds no rule, the server asks its classifier (if
// it can propose) and reports where the rule came from; the proposal is still
// verified by the parser before it is returned.
func TestProbeSource_ModelFallback(t *testing.T) {
	srv, s := testServer(t)
	srv.SetClassifier(proposingClassifier{cands: []ingest.ProbeExtractor{
		{Field: "none", Pattern: `(?i)id[=:]\s*(\w+)`},
	}})
	handler := srv.Handler()

	name := fmt.Sprintf("probemodel-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	lines := []string{"handled id=ab12 ok", "handled ID: cd34 ok", "handled nothing here"}
	sample, _ := json.Marshal(lines)
	if err := s.UpsertPendingLogGroup(ns.ID, "api", "i", "d", string(sample), `{}`, 3, false); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"field": "trace_id",
		"base":  map[string]any{"format": "text"},
		"marks": []map[string]any{
			{"line": 0, "prefix": "handled id=", "text": "ab12"},
			{"line": 1, "prefix": "handled ID: ", "text": "cd34"},
		},
	})
	req := httptest.NewRequest("POST", "/api/sources/ns1/api/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("probe = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Source  string `json:"source"`
		Matched int    `json:"matched"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Source != "model" || resp.Matched != 2 {
		t.Errorf("source=%q matched=%d, want model and 2: %s", resp.Source, resp.Matched, w.Body.String())
	}
}

// Every detected candidate can be asked for by index, and the response says
// how many there are, so a client can offer them one after another.
func TestProbeSource_DetectedCandidatesByIndex(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("probecand-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
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
	if err := s.UpsertPendingLogGroup(ns.ID, "api", "i", "d", string(sample), string(detJSON), len(lines), false); err != nil {
		t.Fatal(err)
	}

	probe := func(idx int) (fields struct {
		Extractor *struct {
			Field string `json:"field"`
		} `json:"extractor"`
		DetectedTotal int `json:"detected_total"`
	}) {
		body, _ := json.Marshal(map[string]any{
			"field": "level", "base": map[string]any{"format": "text"},
			"marks": []any{}, "use_detected": true, "detected_index": idx,
		})
		req := httptest.NewRequest("POST", "/api/sources/ns1/api/probe", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("probe = %d: %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		return fields
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
