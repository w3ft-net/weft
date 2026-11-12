//go:build !defaultshape

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// seedPendingSource creates a namespace and a final-sample (StatusPending,
// ready-to-configure) log_group row for it, mirroring what a completed
// weftd/CloudWatch sample handshake leaves behind.
func seedPendingSource(t *testing.T, srv *Server, orgID store.OrgID, namespace string, service string) {
	t.Helper()
	ns, err := srv.store.FindOrCreateNamespace(orgID, namespace)
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	if err := srv.store.UpsertPendingLogGroup(ns.ID, service, "i-test", "test-deploy",
		`["line one", "line two"]`, `{}`, 2, false); err != nil {
		t.Fatalf("seed pending log group: %v", err)
	}
}

// TestConfigureSource_PersistsFullFieldSet pins the fix for a bug found
// while building the CLI's non-interactive configure mode: the endpoint
// used to accept and persist only a subset of ingest.Schema's fields
// (format/timestamp_field/timestamp_format/level_field/level_pattern/
// trace_id_field/expand_keys) — envelope, field_separator,
// timestamp_fields, and json_start_field were silently dropped even
// though the interactive setup wizard (cmd/weft/setup) detects and sets
// all of them. A source configured with prefix_json + json_start_field
// would persist a schema that never extracts a payload at all
// (extractPayload returns nil when JSONStartField < 1). This asserts
// every field on the request survives into the stored schema_yaml.
func TestConfigureSource_PersistsFullFieldSet(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("cfgsrc-%d", os.Getpid()))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("cfgsrc-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, fmt.Sprintf("cfgsrc-%d", os.Getpid()))
	jwt, _ := auth.Generate(user, "test-secret")

	seedPendingSource(t, srv, cust.ID, "ns1", "worker")

	body, _ := json.Marshal(map[string]any{
		"envelope":               "kevcons",
		"format":                 "prefix_json",
		"field_separator":        "|",
		"timestamp_field":        "1",
		"timestamp_format":       "2006/01/02 15:04:05",
		"timestamp_fields":       2,
		"level_field":            "3",
		"level_pattern":          "^(\\w+)/",
		"trace_id_field":         "5",
		"trace_id_pattern":       "trace_id=(\\S+)",
		"span_name_field":        "6",
		"span_name_pattern":      "name=(\\S+)",
		"span_id_field":          "7",
		"span_id_pattern":        "span_id=(\\S+)",
		"parent_span_id_field":   "8",
		"parent_span_id_pattern": "parent_span_id=(\\S*)",
		"duration_ns_field":      "9",
		"duration_ns_pattern":    "duration_ns=(\\d+)",
		"json_start_field":       4,
		"expand_keys":            []map[string]string{{"key": "status", "type": "INTEGER"}},
	})
	req := httptest.NewRequest("POST", "/api/sources/ns1/worker/configure", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("configure: %d %s", w.Code, w.Body.String())
	}

	lg, err := s.FindLogGroup(cust.ID, "ns1", "worker")
	if err != nil {
		t.Fatalf("read back log group: %v", err)
	}
	yamlStr := lg.SchemaYAML
	for _, want := range []string{
		"envelope: kevcons",
		"format: prefix_json",
		"field_separator: '|'",
		"timestamp_field: \"1\"",
		"timestamp_fields: 2",
		"trace_id_field: \"5\"",
		"trace_id_pattern: trace_id=(\\S+)",
		"span_name_field: \"6\"",
		"span_name_pattern: name=(\\S+)",
		"span_id_field: \"7\"",
		"span_id_pattern: span_id=(\\S+)",
		"parent_span_id_field: \"8\"",
		"parent_span_id_pattern: parent_span_id=(\\S*)",
		"duration_ns_field: \"9\"",
		"duration_ns_pattern: duration_ns=(\\d+)",
		"json_start_field: 4",
		"key: status",
		"type: INTEGER",
	} {
		if !strings.Contains(yamlStr, want) {
			t.Errorf("persisted schema_yaml missing %q; got:\n%s", want, yamlStr)
		}
	}
}

// TestConfigureSource_RejectsInvalidSchema pins the server-side
// validation added alongside the CLI's non-interactive mode: a caller
// that bypasses the CLI (MCP, a hand-rolled script) must not be able to
// silently persist a schema that parse.go would extract nothing from.
func TestConfigureSource_RejectsInvalidSchema(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("cfgbad-%d", os.Getpid()))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("cfgbad-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, fmt.Sprintf("cfgbad-%d", os.Getpid()))
	jwt, _ := auth.Generate(user, "test-secret")

	seedPendingSource(t, srv, cust.ID, "ns2", "worker")

	body, _ := json.Marshal(map[string]any{
		"format": "prefix_json", // missing json_start_field
	})
	req := httptest.NewRequest("POST", "/api/sources/ns2/worker/configure", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "json_start_field") {
		t.Errorf("expected error to mention json_start_field, got: %s", w.Body.String())
	}

	// The invalid request must not have flipped the source to configured.
	lg, err := s.FindLogGroup(cust.ID, "ns2", "worker")
	if err != nil {
		t.Fatalf("read back log group: %v", err)
	}
	if lg.Status != "pending" {
		t.Errorf("expected source to remain pending after rejected configure, got %q", lg.Status)
	}
}

// A schema built by the wizard carries extract specs (what to strip around a
// value, where to cut it, a fallback); configure must persist them, or the
// saved schema silently reads values the legacy way.
func TestConfigureSource_PersistsExtractSpecs(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("cfgextract-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")
	seedPendingSource(t, srv, cust.ID, "ns1", "worker")
	// The source's agent is capable of reading extract specs (a weftd source
	// with no reporting agent is refused; see sources_agent_guard_test.go).
	if lg, _ := s.FindLogGroup(cust.ID, "ns1", "worker"); lg != nil {
		if err := s.RecordAgentSeen(lg.ID, "web-1", ingest.MinAgentVersionForExtract); err != nil {
			t.Fatal(err)
		}
	}

	post := func(body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/sources/ns1/worker/configure", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	code, out := post(map[string]any{
		"format":           "text",
		"timestamp_field":  "1",
		"timestamp_fields": 2,
		"timestamp_format": "2006-01-02 15:04:05,000",
		"level_field":      "3",
		"trace_id_field":   "-1",
		"extract": map[string]any{
			"timestamp": map[string]any{"prefix": "[", "suffix": ":"},
			"level":     map[string]any{"cut_at": "/", "suffix": "]"},
			"trace_id":  map[string]any{"prefix": "trace_id=", "fallback": `id[=:](\w+)`},
		},
	})
	if code != 200 {
		t.Fatalf("configure = %d: %s", code, out)
	}
	lg, _ := s.FindLogGroup(cust.ID, "ns1", "worker")
	for _, want := range []string{"extract:", "cut_at: /", "prefix: trace_id=", "trace_id_field: \"-1\""} {
		if !strings.Contains(lg.SchemaYAML, want) {
			t.Errorf("stored schema missing %q:\n%s", want, lg.SchemaYAML)
		}
	}

	// The stored schema parses with the spans the wizard showed.
	schema, err := ingest.LoadSchemaFromBytes([]byte(lg.SchemaYAML))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ingest.NewParser(schema)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := p.ParseLine("[2026-10-15 12:00:03,425: INFO/MainProcess] hello trace_id=abc123", time.Unix(0, 1))
	if row.Level != "INFO" || row.TraceId != "abc123" || row.TsUnixNs == 1 {
		t.Errorf("row = level %q trace %q ts %d", row.Level, row.TraceId, row.TsUnixNs)
	}

	// An invalid spec is rejected like any other bad schema.
	if code, _ := post(map[string]any{"format": "text", "extract": map[string]any{"colour": map[string]any{}}}); code != 400 {
		t.Errorf("unknown extract value = %d, want 400", code)
	}
}
