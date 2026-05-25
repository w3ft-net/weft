package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// herokuFrame builds an octet-counted RFC5424 frame the way Heroku's
// drain protocol delivers them: `<bytes> <message>` where `<bytes>`
// is the exact byte count of the message that follows the single
// space.
func herokuFrame(msg string) string {
	return fmt.Sprintf("%d %s", len(msg), msg)
}

// hbody concatenates frames for a multi-frame body.
func hbody(frames ...string) string {
	return strings.Join(frames, "")
}

// TestHerokuAdapter_Auth_PathToken: the {token} URL path component is
// the authoritative bearer for a Heroku drain. http.ServeMux's
// pattern syntax (Go 1.22+) populates req.PathValue("token"); the
// fallback for path-stripping reverse proxies takes the last
// non-empty segment.
func TestHerokuAdapter_Auth_PathToken(t *testing.T) {
	a := HerokuAdapter{}

	// With ServeMux pattern routing.
	mux := http.NewServeMux()
	var got string
	mux.Handle("POST /api/ingest/heroku/{token}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := a.Auth(r)
		if err != nil {
			t.Fatal(err)
		}
		got = token
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/wft_abcdef", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if got != "wft_abcdef" {
		t.Errorf("token = %q, want wft_abcdef", got)
	}
}

// TestHerokuAdapter_Auth_Missing: empty path → explicit error so the
// shared handler returns 401 rather than silently passing "" to
// ValidateAgentToken.
func TestHerokuAdapter_Auth_Missing(t *testing.T) {
	a := HerokuAdapter{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, err := a.Auth(req); err == nil {
		t.Error("expected error for missing token")
	}
}

// TestHerokuAdapter_Parse_AppDynoSplit: appname=myapp, procid=web.1
// → app=myapp/web instance=web.1. Strips the dyno number from the
// app name (so all dynos of one type land in the same pseudofile)
// while keeping it on instance for cross-dyno aggregations.
func TestHerokuAdapter_Parse_AppDynoSplit(t *testing.T) {
	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - State changed from starting to up`
	body := hbody(herokuFrame(msg))

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got, want := rows[0].Service+"/"+rows[0].Kind, "myapp/web"; got != want {
		t.Errorf("App = %q, want %q (dyno number must drop from app name)", got, want)
	}
	if got, want := rows[0].Instance, "web.1"; got != want {
		t.Errorf("Instance = %q, want %q (dyno number stays on instance)", got, want)
	}
	if got, want := rows[0].Raw, "State changed from starting to up"; got != want {
		t.Errorf("Raw = %q, want %q", got, want)
	}
	if rows[0].TsUnixNs == 0 {
		t.Error("TsUnixNs unset; RFC5424 timestamp should parse")
	}
}

// TestHerokuAdapter_Parse_Router: appname=myapp procid=router →
// app=myapp/router. Heroku's router emits one log line per HTTP
// request; users want to grep that stream independently of app stdout.
func TestHerokuAdapter_Parse_Router(t *testing.T) {
	msg := `<134>1 2026-04-30T12:00:01.500000+00:00 host myapp router - - at=info method=GET path="/api/users" host=myapp.herokuapp.com status=200 bytes=512 protocol=https`
	body := hbody(herokuFrame(msg))

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Service+"/"+rows[0].Kind != "myapp/router" {
		t.Errorf("App = %q, want myapp/router", rows[0].Service+"/"+rows[0].Kind)
	}
	if rows[0].Instance != "router" {
		t.Errorf("Instance = %q, want router (no dyno number)", rows[0].Instance)
	}
}

// TestHerokuAdapter_Parse_HerokuPlatformEvents: appname=heroku
// (platform-emitted) → split host into the canonical app name and
// rewrite procid to "platform" so deploys/restarts/scales land in
// <app>/platform regardless of which dyno emitted them.
func TestHerokuAdapter_Parse_HerokuPlatformEvents(t *testing.T) {
	msg := `<134>1 2026-04-30T12:00:02.000000+00:00 myapp heroku api - - Release v42 created by user@example.com`
	body := hbody(herokuFrame(msg))

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Service+"/"+rows[0].Kind != "myapp/platform" {
		t.Errorf("App = %q, want myapp/platform (heroku-originated event must surface as platform sub-app)", rows[0].Service+"/"+rows[0].Kind)
	}
}

// TestHerokuAdapter_Parse_MultiFrame: a single POST carries multiple
// frames; the parser walks every one. Mirrors what Heroku actually
// delivers (typically tens of frames per batch).
func TestHerokuAdapter_Parse_MultiFrame(t *testing.T) {
	body := hbody(
		herokuFrame(`<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - first`),
		herokuFrame(`<134>1 2026-04-30T12:00:01.000000+00:00 host myapp web.1 - - second`),
		herokuFrame(`<134>1 2026-04-30T12:00:02.000000+00:00 host myapp worker.1 - - third`),
	)

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].Raw != "first" || rows[1].Raw != "second" || rows[2].Raw != "third" {
		t.Errorf("rows raw = [%q, %q, %q], want [first, second, third]", rows[0].Raw, rows[1].Raw, rows[2].Raw)
	}
	if rows[2].Service+"/"+rows[2].Kind != "myapp/worker" {
		t.Errorf("rows[2] service/kind = %q, want myapp/worker", rows[2].Service+"/"+rows[2].Kind)
	}
}

// TestHerokuAdapter_Parse_L2metMetricLine: the runtime-heroku-metrics
// labs feature emits l2met-shaped lines. The adapter detects them and
// emits TWO rows: the canonical (myapp/web) row plus a metrics row
// routed to myapp/metrics with the parsed key/value payload. Both
// rows share the same timestamp, instance, and raw line — the
// metrics row is the queryable, structured form, the canonical row
// is the literal-grep form.
func TestHerokuAdapter_Parse_L2metMetricLine(t *testing.T) {
	line := `source=web.1 dyno=heroku.123.uuid sample#memory_total=512.00MB sample#memory_rss=487.21MB sample#load_avg_1m=0.42`
	msg := fmt.Sprintf(`<134>1 2026-04-30T12:00:03.000000+00:00 host myapp web.1 - - %s`, line)
	body := hbody(herokuFrame(msg))

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (canonical + metrics)", len(rows))
	}
	if rows[0].Service+"/"+rows[0].Kind != "myapp/web" {
		t.Errorf("rows[0] service/kind = %q, want myapp/web (canonical row)", rows[0].Service+"/"+rows[0].Kind)
	}
	if rows[1].Service+"/"+rows[1].Kind != "myapp/metrics" {
		t.Errorf("rows[1] service/kind = %q, want myapp/metrics", rows[1].Service+"/"+rows[1].Kind)
	}
	if len(rows[1].PayloadJson) == 0 {
		t.Fatal("metrics row has no PayloadJson")
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[1].PayloadJson, &payload); err != nil {
		t.Fatalf("PayloadJson not valid JSON: %v (%s)", err, rows[1].PayloadJson)
	}
	// Numeric values parse as JSON numbers.
	if v, ok := payload["load_avg_1m"].(float64); !ok || v != 0.42 {
		t.Errorf("payload.load_avg_1m = %v, want 0.42 as a number", payload["load_avg_1m"])
	}
	// Values with trailing units split into <key> + <key>_unit so the
	// numeric is queryable AND the unit survives.
	if v, ok := payload["memory_total"].(float64); !ok || v != 512.00 {
		t.Errorf("payload.memory_total = %v, want 512.00 (numeric core)", payload["memory_total"])
	}
	if u, _ := payload["memory_total_unit"].(string); u != "MB" {
		t.Errorf("payload.memory_total_unit = %q, want MB", u)
	}
	// source identity is preserved as a non-metric key.
	if s, _ := payload["source"].(string); s != "web.1" {
		t.Errorf("payload.source = %q, want web.1", s)
	}
}

// TestHerokuAdapter_Parse_NotL2met: a regular log line that contains
// `source=` but no `(sample|measure|count|bucket)#` segments must NOT
// emit a metrics row. Router logs in particular include
// `source=foo.com` patterns that would false-positive without the
// metric-prefix gate.
func TestHerokuAdapter_Parse_NotL2met(t *testing.T) {
	line := `at=info method=GET source=ip=1.2.3.4 status=200`
	msg := fmt.Sprintf(`<134>1 2026-04-30T12:00:04.000000+00:00 host myapp router - - %s`, line)
	body := hbody(herokuFrame(msg))

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (no metrics row for non-l2met lines)", len(rows))
	}
}

// TestHerokuAdapter_Parse_MalformedFrameSkipped: Heroku occasionally
// delivers garbage during drain reconfiguration. A bad RFC5424 header
// should be skipped rather than failing the whole batch — the shared
// handler returning 4xx would surface in `heroku drains` as a
// permanent delivery failure, which is the wrong UX. Subsequent
// well-formed frames in the same body must still get through.
func TestHerokuAdapter_Parse_MalformedFrameSkipped(t *testing.T) {
	body := hbody(
		herokuFrame(`<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - - good`),
		herokuFrame(`not actually rfc5424`),
		herokuFrame(`<134>1 2026-04-30T12:00:02.000000+00:00 host myapp web.1 - - also good`),
	)

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2 (malformed middle frame skipped)", len(rows))
	}
}

// TestHerokuAdapter_ContentLimit: 1 MB matches Heroku's drain payload
// cap. Codified as a constant-shaped assertion so a refactor to push
// the limit higher (and inadvertently expose the coordinator to
// abuse) trips the test.
func TestHerokuAdapter_ContentLimit(t *testing.T) {
	a := HerokuAdapter{}
	if got, want := a.ContentLimit(), int64(1<<20); got != want {
		t.Errorf("ContentLimit = %d, want %d (1 MB matches Heroku's cap)", got, want)
	}
}

// TestHerokuAdapter_Parse_StructuredDataSkipped: RFC5424's structured-
// data segment is `[id key="val"...]` between msgid and the message.
// Heroku writes "-" today but a future protocol bump might add it;
// the parser should treat the segment as opaque and recover the
// message body that follows.
func TestHerokuAdapter_Parse_StructuredDataSkipped(t *testing.T) {
	msg := `<134>1 2026-04-30T12:00:00.000000+00:00 host myapp web.1 - [origin@1234 enterpriseId="someid"] hello world`
	body := hbody(herokuFrame(msg))

	a := HerokuAdapter{}
	rows, err := a.Parse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Raw != "hello world" {
		t.Errorf("Raw = %q, want %q (structured-data segment must be stripped)", rows[0].Raw, "hello world")
	}
}
