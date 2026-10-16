package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/ingest"
)

// weftd parses on the agent, so a schema is only as good as the oldest agent
// reading it. An agent that cannot interpret an extract-form schema ignores
// it silently (rows keep flowing with no level, no trace id), so the schema is
// refused when it is stored instead.
func TestConfigureSource_ExtractSchemaNeedsCapableAgents(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("agentguard-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	extractForm := map[string]any{
		"format": "text", "level_field": "3",
		"extract": map[string]any{"level": map[string]any{"cut_at": "/"}},
	}
	legacyForm := map[string]any{"format": "text", "level_field": "3"}

	configure := func(service string, body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/sources/ns1/"+service+"/configure", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	record := func(service, instance, version string) {
		t.Helper()
		lg, err := s.FindLogGroup(cust.ID, "ns1", service)
		if err != nil || lg == nil {
			t.Fatalf("FindLogGroup %s: %v %v", service, lg, err)
		}
		if err := s.RecordAgentSeen(lg.ID, instance, version); err != nil {
			t.Fatal(err)
		}
	}
	min := ingest.MinAgentVersionForExtract

	// No agent has ever reported: unknown, so refused, with the reason.
	seedPendingSource(t, srv, cust.ID, "ns1", "unknown")
	if code, body := configure("unknown", extractForm); code != 409 || !strings.Contains(body, "no agent") {
		t.Errorf("unknown agents = %d %q, want 409 saying no agent has reported", code, body)
	}

	// An old agent: refused, naming the version it has and the one it needs.
	seedPendingSource(t, srv, cust.ID, "ns1", "old")
	record("old", "web-1", "0.23.0")
	if code, body := configure("old", extractForm); code != 409 || !strings.Contains(body, "0.23.0") || !strings.Contains(body, min) {
		t.Errorf("old agent = %d %q, want 409 naming 0.23.0 and %s", code, body, min)
	}

	// One old host among new ones still blocks it: all of them read the schema.
	seedPendingSource(t, srv, cust.ID, "ns1", "mixed")
	record("mixed", "web-1", min)
	record("mixed", "web-2", "0.23.0")
	if code, _ := configure("mixed", extractForm); code != 409 {
		t.Errorf("mixed fleet = %d, want 409", code)
	}

	// A retired old host that has gone quiet does not block it forever.
	seedPendingSource(t, srv, cust.ID, "ns1", "retired")
	record("retired", "web-1", min)
	record("retired", "gone", "0.20.0")
	if err := s.DB().Exec(`UPDATE log_group_agents SET seen_at = now() - interval '30 days' WHERE instance = 'gone'`).Error; err != nil {
		t.Fatal(err)
	}
	if code, body := configure("retired", extractForm); code != 200 {
		t.Errorf("retired old host = %d %q, want 200", code, body)
	}

	// Every agent capable: stored.
	seedPendingSource(t, srv, cust.ID, "ns1", "fleet")
	record("fleet", "web-1", min)
	record("fleet", "web-2", "0.29.1")
	if code, body := configure("fleet", extractForm); code != 200 {
		t.Errorf("capable fleet = %d %q, want 200", code, body)
	}

	// A legacy-form schema needs nothing of the agent.
	seedPendingSource(t, srv, cust.ID, "ns1", "legacy")
	if code, body := configure("legacy", legacyForm); code != 200 {
		t.Errorf("legacy form with unknown agents = %d %q, want 200", code, body)
	}

	// A negative field index is extract-form even with no extract section.
	seedPendingSource(t, srv, cust.ID, "ns1", "negative")
	if code, _ := configure("negative", map[string]any{"format": "text", "trace_id_field": "-1", "extract": map[string]any{"trace_id": map[string]any{}}}); code != 409 {
		t.Errorf("negative index with unknown agents = %d, want 409", code)
	}
}

// Push sources are parsed by the API, not an agent, so there is no agent to
// ask about.
func TestConfigureSource_PushSourcesAreNotGuarded(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	name := fmt.Sprintf("agentguard-push-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	if err := s.EnsureServiceLogGroup(ns.ID, "direct"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"format": "text", "level_field": "3",
		"extract": map[string]any{"level": map[string]any{"cut_at": "/"}},
	})
	req := httptest.NewRequest("POST", "/api/sources/ns1/direct/configure", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("a push source = %d %q, want 200", w.Code, w.Body.String())
	}
}

// `weft source list` shows who is reading each weftd source: how many hosts
// reported and the oldest and newest versions among them.
func TestListSources_ReportsAgentFleet(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	name := fmt.Sprintf("agentlist-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	seedPendingSource(t, srv, cust.ID, "ns1", "fleet")
	seedPendingSource(t, srv, cust.ID, "ns1", "silent")
	lg, _ := s.FindLogGroup(cust.ID, "ns1", "fleet")
	for _, a := range [][2]string{{"web-1", "0.28.0"}, {"web-2", "0.23.0"}, {"web-3", "0.27.0"}} {
		if err := s.RecordAgentSeen(lg.ID, a[0], a[1]); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/api/sources?namespace=ns1", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	var out []struct {
		Service string `json:"service"`
		Agents  *struct {
			Count  int    `json:"count"`
			Oldest string `json:"oldest"`
			Newest string `json:"newest"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byService := map[string]int{}
	for i, o := range out {
		byService[o.Service] = i
	}
	fleet := out[byService["fleet"]]
	if fleet.Agents == nil || fleet.Agents.Count != 3 || fleet.Agents.Oldest != "0.23.0" || fleet.Agents.Newest != "0.28.0" {
		t.Errorf("fleet agents = %+v, want 3 hosts, 0.23.0 to 0.28.0", fleet.Agents)
	}
	if silent := out[byService["silent"]]; silent.Agents != nil && silent.Agents.Count != 0 {
		t.Errorf("a source no agent has reported for shows %+v", silent.Agents)
	}
}

// duration_unit changes how the agent turns text into nanoseconds; an older
// agent would read "5.01s" as no duration at all, so the schema is refused
// until every agent reading the source is new enough. Plain nanoseconds read
// the same everywhere and need nothing.
func TestConfigureSource_DurationUnitNeedsCapableAgents(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("agentguard-duration-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	form := func(unit string) map[string]any {
		return map[string]any{"format": "text", "duration_ns_field": "none", "duration_ns_pattern": `took=(\S+)`, "duration_unit": unit}
	}
	configure := func(service string, body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/sources/ns1/"+service+"/configure", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	record := func(service, version string) {
		t.Helper()
		lg, err := s.FindLogGroup(cust.ID, "ns1", service)
		if err != nil || lg == nil {
			t.Fatalf("FindLogGroup %s: %v %v", service, lg, err)
		}
		if err := s.RecordAgentSeen(lg.ID, "web-1", version); err != nil {
			t.Fatal(err)
		}
	}
	min := ingest.MinAgentVersionForDurationUnit

	// An agent that has extract specs but not duration units: refused, naming both.
	seedPendingSource(t, srv, cust.ID, "ns1", "old")
	record("old", ingest.MinAgentVersionForExtract)
	if code, body := configure("old", form("ms")); code != 409 || !strings.Contains(body, min) {
		t.Errorf("old agent with ms = %d %q, want 409 naming %s", code, body, min)
	}

	// The same agent reads whole nanoseconds fine.
	seedPendingSource(t, srv, cust.ID, "ns1", "ns")
	record("ns", ingest.MinAgentVersionForExtract)
	if code, body := configure("ns", form("ns")); code != 200 {
		t.Errorf("old agent with ns = %d %q, want 200", code, body)
	}

	// A capable agent: stored.
	seedPendingSource(t, srv, cust.ID, "ns1", "new")
	record("new", min)
	if code, body := configure("new", form("text")); code != 200 {
		t.Errorf("capable agent with text = %d %q, want 200", code, body)
	}

	// An unknown unit is invalid, whoever reads it.
	seedPendingSource(t, srv, cust.ID, "ns1", "bad")
	record("bad", min)
	if code, _ := configure("bad", form("fortnights")); code != 400 {
		t.Errorf("unknown unit = %d, want 400", code)
	}
}
