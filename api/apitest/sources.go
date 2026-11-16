package apitest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/ingest"
	"github.com/w3ft-net/weft/sources"
)

// Sources runs the routes that take a source from sampling to configured: the
// pending list and its count, the live progress of a sample, configuring a
// schema, and sampling again.
func Sources(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"ConfigurePersistsTheFullFieldSet", configurePersistsFullFieldSet},
		{"ConfigureRejectsAnInvalidSchema", configureRejectsInvalidSchema},
		{"ConfigurePersistsExtractSpecs", configurePersistsExtractSpecs},
		{"ResamplePending", resamplePending},
		{"ResampleSize", resampleSize},
		{"ResampleOfAConfiguredSourceIsVisible", resampleOfConfiguredIsVisible},
		{"SamplingProgress", samplingProgress},
		{"PendingCount", pendingCount},
	})
}

// seedPending is a source with a complete first sample, awaiting a schema: what
// an agent's final sample leaves behind.
func (s Subject) seedPending(t *testing.T, namespace, service string) uint {
	t.Helper()
	ns := s.namespace(t, namespace)
	if err := s.Sources.SubmitSample(ns, service, source.SampleSubmission{
		Instance: "i-test", Deployment: "test-deploy", SampleJSON: `["line one", "line two"]`, DetectionJSON: `{}`, Collected: 2,
	}); err != nil {
		t.Fatalf("seed pending source: %v", err)
	}
	return ns
}

func (s Subject) source(t *testing.T, namespace, service string) *source.Source {
	t.Helper()
	src, err := s.Sources.FindSource(s.Owner, namespace, service)
	if err != nil || src == nil {
		t.Fatalf("FindSource(%s/%s) = %v, %v", namespace, service, src, err)
	}
	return src
}

func (s Subject) get(t *testing.T, path string, into any) {
	t.Helper()
	w := s.Do("GET", path, nil)
	if w.Code != 200 {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	decode(t, w, into)
}

// The configure route persists every field ingest.Schema has. It once kept a
// subset, so a source configured with prefix_json and json_start_field stored a
// schema that never extracted a payload at all.
func configurePersistsFullFieldSet(t *testing.T, s Subject) {
	s.seedPending(t, "ns1", "worker")
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
	if w := s.Do("POST", "/api/sources/ns1/worker/configure", body); w.Code != 200 {
		t.Fatalf("configure: %d %s", w.Code, w.Body.String())
	}
	yaml := s.source(t, "ns1", "worker").SchemaYAML
	for _, want := range []string{
		"envelope: kevcons", "format: prefix_json", "field_separator: '|'", "timestamp_field: \"1\"",
		"timestamp_fields: 2", "trace_id_field: \"5\"", "trace_id_pattern: trace_id=(\\S+)",
		"span_name_field: \"6\"", "span_name_pattern: name=(\\S+)", "span_id_field: \"7\"",
		"span_id_pattern: span_id=(\\S+)", "parent_span_id_field: \"8\"", "parent_span_id_pattern: parent_span_id=(\\S*)",
		"duration_ns_field: \"9\"", "duration_ns_pattern: duration_ns=(\\d+)", "json_start_field: 4",
		"key: status", "type: INTEGER",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("persisted schema_yaml missing %q; got:\n%s", want, yaml)
		}
	}
}

// A caller that bypasses the CLI must not be able to persist a schema the
// parser would extract nothing from.
func configureRejectsInvalidSchema(t *testing.T, s Subject) {
	s.seedPending(t, "ns2", "worker")
	body, _ := json.Marshal(map[string]any{"format": "prefix_json"}) // missing json_start_field
	w := s.Do("POST", "/api/sources/ns2/worker/configure", body)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "json_start_field") {
		t.Errorf("expected the error to mention json_start_field, got: %s", w.Body.String())
	}
	if got := s.source(t, "ns2", "worker").Status; got != sources.Pending {
		t.Errorf("source is %q after a rejected configure, want it to stay pending", got)
	}
}

// A schema built by the wizard carries extract specs (what to strip around a
// value, where to cut it, a fallback); configure must persist them, or the saved
// schema silently reads values the legacy way.
func configurePersistsExtractSpecs(t *testing.T, s Subject) {
	if s.RecordAgentSeen == nil {
		// Configure checks the versions of the agents that read a source before it
		// accepts a spec they might not understand; a build with no record of its
		// agents answers 409 "could not check which agent versions read this
		// source".
		t.Skip("this build keeps no record of its agents")
	}
	s.seedPending(t, "ns1", "worker")
	post := func(body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		w := s.Do("POST", "/api/sources/ns1/worker/configure", b)
		return w.Code, w.Body.String()
	}
	// A source whose agent is too old to read extract specs is refused.
	s.RecordAgentSeen(t, s.source(t, "ns1", "worker").ID, "web-1", ingest.MinAgentVersionForExtract)
	code, out := post(map[string]any{
		"format": "text", "timestamp_field": "1", "timestamp_fields": 2, "timestamp_format": "2006-01-02 15:04:05,000",
		"level_field": "3", "trace_id_field": "-1",
		"extract": map[string]any{
			"timestamp": map[string]any{"prefix": "[", "suffix": ":"},
			"level":     map[string]any{"cut_at": "/", "suffix": "]"},
			"trace_id":  map[string]any{"prefix": "trace_id=", "fallback": `id[=:](\w+)`},
		},
	})
	if code != 200 {
		t.Fatalf("configure = %d: %s", code, out)
	}
	yaml := s.source(t, "ns1", "worker").SchemaYAML
	for _, want := range []string{"extract:", "cut_at: /", "prefix: trace_id=", "trace_id_field: \"-1\""} {
		if !strings.Contains(yaml, want) {
			t.Errorf("stored schema missing %q:\n%s", want, yaml)
		}
	}
	schema, err := ingest.LoadSchemaFromBytes([]byte(yaml))
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
	if code, _ := post(map[string]any{"format": "text", "extract": map[string]any{"colour": map[string]any{}}}); code != 400 {
		t.Errorf("unknown extract value = %d, want 400", code)
	}
}

// A pending source can be sent back to sampling over HTTP, and one still on its
// first sample answers 409 instead of silently succeeding.
func resamplePending(t *testing.T, s Subject) {
	s.seedPending(t, "ns1", "worker")
	code := func(path string) int { return s.Do("POST", path, nil).Code }
	if c := code("/api/sources/ns1/worker/resample"); c != 200 {
		t.Fatalf("resample pending = %d, want 200", c)
	}
	src := s.source(t, "ns1", "worker")
	if src.Status != sources.Sampling || src.Sample != "" || src.ResampleRequestedAt == nil {
		t.Errorf("status=%q sample=%q flag=%v, want sampling, cleared, flagged", src.Status, src.Sample, src.ResampleRequestedAt)
	}
	if c := code("/api/sources/ns1/worker/resample"); c != 409 {
		t.Errorf("resample while sampling = %d, want 409", c)
	}
	if c := code("/api/sources/ns1/nope/resample"); c != 404 {
		t.Errorf("resample unknown source = %d, want 404", c)
	}
}

// ?size=N on a resample sets how many lines the new sample collects, and the
// source's progress reports that target instead of 50.
func resampleSize(t *testing.T, s Subject) {
	s.seedPending(t, "ns1", "worker")
	for _, bad := range []string{"abc", "10", "999999"} {
		if w := s.Do("POST", "/api/sources/ns1/worker/resample?size="+bad, nil); w.Code != 400 {
			t.Errorf("size=%s = %d, want 400", bad, w.Code)
		}
	}
	if w := s.Do("POST", "/api/sources/ns1/worker/resample?size=500", nil); w.Code != 200 {
		t.Fatalf("size=500 = %d: %s", w.Code, w.Body)
	}
	var got struct {
		SampleTarget int `json:"sample_target"`
	}
	s.get(t, "/api/sources/ns1/worker/schema", &got)
	if got.SampleTarget != 500 {
		t.Errorf("sample_target = %d, want 500", got.SampleTarget)
	}
}

// A configured source being resampled keeps ingesting, so its row stays
// "configured". The user still has to see it: the pending list carries it
// (sampling while collecting, pending once the fresh sample is ready) and the
// source list reports the resample's progress.
func resampleOfConfiguredIsVisible(t *testing.T, s Subject) {
	ns := s.namespace(t, "ns1")
	submit := func(sample string, collected int, partial bool) {
		t.Helper()
		if err := s.Sources.SubmitSample(ns, "api", source.SampleSubmission{Instance: "i", Deployment: "d", SampleJSON: sample, DetectionJSON: `{}`, Collected: collected, Partial: partial}); err != nil {
			t.Fatal(err)
		}
	}
	submit(`["a"]`, 1, false)
	if err := s.Sources.ConfigureSource(context.Background(), ns, "api", "app: api\nformat: text\n"); err != nil {
		t.Fatal(err)
	}
	type pending struct {
		Service         string `json:"service"`
		Status          string `json:"status"`
		SampleCollected int    `json:"sample_collected"`
	}
	type listed struct {
		Service  string `json:"service"`
		Status   string `json:"status"`
		Resample *struct {
			Collected int  `json:"collected"`
			Target    int  `json:"target"`
			Ready     bool `json:"ready"`
		} `json:"resample"`
	}
	find := func(rows []listed) *listed {
		for i := range rows {
			if rows[i].Service == "api" {
				return &rows[i]
			}
		}
		return nil
	}
	read := func() ([]pending, []listed) {
		var p []pending
		var l []listed
		s.get(t, "/api/sources/pending", &p)
		s.get(t, "/api/sources", &l)
		return p, l
	}
	if p, l := read(); len(p) != 0 || find(l) == nil || find(l).Resample != nil {
		t.Fatalf("before resample: pending=%v listed=%+v, want nothing pending and no resample", p, find(l))
	}
	if err := s.Sources.RequestResample(ns, "api", 0); err != nil {
		t.Fatal(err)
	}
	submit(`["f1","f2"]`, 2, true)
	p, l := read()
	if len(p) != 1 || p[0].Service != "api" || p[0].Status != "sampling" || p[0].SampleCollected != 2 {
		t.Errorf("mid-resample pending = %+v, want api sampling 2", p)
	}
	row := find(l)
	if row == nil || row.Status != "configured" || row.Resample == nil ||
		row.Resample.Collected != 2 || row.Resample.Target != source.SampleTarget || row.Resample.Ready {
		t.Errorf("mid-resample list row = %+v, want configured + resample 2/%d not ready", row, source.SampleTarget)
	}
	submit(`["f1","f2","f3"]`, 3, false)
	p, l = read()
	if len(p) != 1 || p[0].Status != "pending" {
		t.Errorf("finished resample pending = %+v, want api pending", p)
	}
	if row := find(l); row == nil || row.Resample == nil || !row.Resample.Ready {
		t.Errorf("finished resample list row = %+v, want resample.ready", row)
	}
}

// A still-filling sample is watchable: the pending list and the per-source
// schema endpoint carry the lines seen so far and when the newest arrived, so a
// UI or the CLI can render live progress.
func samplingProgress(t *testing.T, s Subject) {
	ns := s.namespace(t, "ns1")
	before := time.Now().Add(-time.Second)
	if err := s.Sources.SubmitSample(ns, "cron", source.SampleSubmission{Instance: "i-test", Deployment: "d", SampleJSON: `["line one","line two"]`, DetectionJSON: `{}`, Collected: 2, Partial: true}); err != nil {
		t.Fatal(err)
	}
	type view struct {
		Status          string   `json:"status"`
		SampleCollected int      `json:"sample_collected"`
		Sample          []string `json:"sample"`
		SampleUpdatedAt string   `json:"sample_updated_at"`
	}
	check := func(where string, v view) {
		t.Helper()
		if v.Status != "sampling" || v.SampleCollected != 2 {
			t.Errorf("%s: status=%q collected=%d, want sampling/2", where, v.Status, v.SampleCollected)
		}
		if len(v.Sample) != 2 || v.Sample[1] != "line two" {
			t.Errorf("%s: sample = %v, want the two lines seen so far", where, v.Sample)
		}
		at, err := time.Parse(time.RFC3339, v.SampleUpdatedAt)
		if err != nil || at.Before(before.Truncate(time.Second)) {
			t.Errorf("%s: sample_updated_at = %q (%v), want a fresh RFC3339 time", where, v.SampleUpdatedAt, err)
		}
	}
	var list []view
	s.get(t, "/api/sources/pending", &list)
	if len(list) != 1 {
		t.Fatalf("pending list = %d rows, want 1", len(list))
	}
	check("pending list", list[0])
	var one view
	s.get(t, "/api/sources/ns1/cron/schema", &one)
	check("schema endpoint", one)
}

// The sidebar polls the count every 30s per tab and only needs a number: it once
// fetched the full pending list, samples and detections included, to take its
// length. A sampling source counts, like it does in the list.
func pendingCount(t *testing.T, s Subject) {
	count := func() int {
		t.Helper()
		var resp struct {
			Count int `json:"count"`
		}
		s.get(t, "/api/sources/pending/count", &resp)
		return resp.Count
	}
	if n := count(); n != 0 {
		t.Fatalf("no sources yet: count = %d, want 0", n)
	}
	s.seedPending(t, "ns1", "worker")
	s.seedPending(t, "ns2", "api")
	ns := s.namespace(t, "ns3")
	if err := s.Sources.SubmitSample(ns, "cron", source.SampleSubmission{Instance: "i-test", Deployment: "d", SampleJSON: `["a"]`, DetectionJSON: `{}`, Collected: 1, Partial: true}); err != nil {
		t.Fatal(err)
	}
	want := 3
	if s.Other != (s.Owner) && s.DoOther != nil {
		// Another group's pending source must not leak into this count.
		other, err := s.Sources.FindOrCreateNamespace(s.Other, "ns1")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Sources.SubmitSample(other.ID, "worker", source.SampleSubmission{Instance: "i-test", Deployment: "d", SampleJSON: `["a"]`, DetectionJSON: `{}`, Collected: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != want {
		t.Fatalf("count = %d, want %d (two pending and one sampling, none from another group)", n, want)
	}
}
