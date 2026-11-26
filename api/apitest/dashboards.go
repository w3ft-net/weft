package apitest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

// Dashboards runs the dashboard routes. A build that serves dashboards wires
// them (Server.SetDashboards) and /etc/dashboards (mounts.Dashboards) over one
// store.
func Dashboards(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"SaveListGetDelete", dashboardRoundTrip},
		{"TilesCarryWhatTheServerComputed", dashboardTilesFilled},
		{"SaveRefusesWhatWouldNotRender", dashboardSaveRefused},
		{"ATileOnADeletedCronIsAPlaceholder", dashboardMissingCron},
	})
}

func putDashboard(s Subject, name, body string) int {
	b, _ := json.Marshal(map[string]string{"yaml_body": body})
	return s.Do("PUT", "/api/dashboards/"+name, b).Code
}

func dashboardRoundTrip(t *testing.T, s Subject) {
	s.saveCron(t, "errors", "echo 1", cron.ValueNumber)
	const body = "title: Ops\ntiles:\n  - {kind: counter, cron: ops/errors}\n"
	if code := putDashboard(s, "ops", body); code != 204 {
		t.Fatalf("PUT = %d", code)
	}

	var list struct {
		Dashboards []struct {
			Name, Title string
			Invalid     bool
		}
	}
	rec := s.Do("GET", "/api/dashboards", nil)
	decode(t, rec, &list)
	if len(list.Dashboards) != 1 || list.Dashboards[0].Name != "ops" || list.Dashboards[0].Title != "Ops" || list.Dashboards[0].Invalid {
		t.Fatalf("list = %+v", list.Dashboards)
	}

	var got struct {
		Name     string
		YAMLBody string `json:"yaml_body"`
		Spec     struct{ Title string }
	}
	rec = s.Do("GET", "/api/dashboards/ops", nil)
	if rec.Code != 200 {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	decode(t, rec, &got)
	if got.Name != "ops" || got.YAMLBody != body || got.Spec.Title != "Ops" {
		t.Fatalf("dashboard = %+v", got)
	}

	if code := s.Do("DELETE", "/api/dashboards/ops", nil).Code; code != 204 {
		t.Fatalf("DELETE = %d", code)
	}
	if code := s.Do("GET", "/api/dashboards/ops", nil).Code; code != 404 {
		t.Errorf("GET after delete = %d, want 404", code)
	}
}

// Clients render what the server computed: a counter's display text and points,
// a sparkline's plot, and the cron's metadata by the path the tile names.
func dashboardTilesFilled(t *testing.T, s Subject) {
	id := s.saveCron(t, "errors", "echo 1", cron.ValueNumber)
	now := time.Now().UTC().Truncate(time.Minute)
	val := func(v float64) *float64 { return &v }
	if err := s.Crons.SaveResults([]cron.Result{
		{WatchID: id, RanAt: now.Add(-2 * time.Hour), Value: val(2)},
		{WatchID: id, RanAt: now.Add(-1 * time.Hour), Value: val(4)},
		{WatchID: id, RanAt: now.Add(-5 * time.Minute), Value: val(7)},
	}); err != nil {
		t.Fatal(err)
	}
	body := "tiles:\n  - {kind: counter, cron: ops/errors, format: '%d errors'}\n  - {kind: sparkline, cron: ops/errors}\n"
	if code := putDashboard(s, "ops", body); code != 204 {
		t.Fatalf("PUT = %d", code)
	}
	var got struct {
		Spec struct {
			Sections []struct {
				Tiles []struct {
					Kind    string
					Display *struct{ Num, After string }
					Points  []struct{ Value *float64 }
					Plot    *struct{ Series []any }
					Error   string
				}
			}
		}
		Crons map[string]*struct {
			Cron struct{ Name, ValueType string }
		}
	}
	rec := s.Do("GET", "/api/dashboards/ops", nil)
	if rec.Code != 200 {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	decode(t, rec, &got)
	tiles := got.Spec.Sections[0].Tiles
	if len(tiles) != 2 {
		t.Fatalf("tiles = %+v", tiles)
	}
	if d := tiles[0].Display; d == nil || d.Num != "7" || d.After != " errors" {
		t.Errorf("counter display = %+v, want 7 errors", d)
	}
	if len(tiles[0].Points) != 1 || tiles[0].Points[0].Value == nil || *tiles[0].Points[0].Value != 7 {
		t.Errorf("counter points = %+v", tiles[0].Points)
	}
	if tiles[1].Plot == nil || len(tiles[1].Plot.Series) == 0 || tiles[1].Error != "" {
		t.Errorf("sparkline plot = %+v error %q", tiles[1].Plot, tiles[1].Error)
	}
	if c := got.Crons["ops/errors"]; c == nil || c.Cron.Name != "errors" || c.Cron.ValueType != cron.ValueNumber {
		t.Errorf("crons = %+v", got.Crons)
	}
}

func dashboardSaveRefused(t *testing.T, s Subject) {
	s.saveCron(t, "holders", "# type: text\necho i-0abc", cron.ValueText)
	for _, tc := range []struct{ name, body, want string }{
		{"missing", "tiles:\n  - {kind: counter, cron: ops/nope}\n", `cron "ops/nope" does not exist`},
		{"text cron", "tiles:\n  - {kind: counter, cron: ops/holders}\n", "a counter tile draws numbers"},
		{"grammar", "tiles:\n  - {kind: pie, cron: ops/holders}\n", "unknown kind"},
	} {
		b, _ := json.Marshal(map[string]string{"yaml_body": tc.body})
		rec := s.Do("PUT", "/api/dashboards/bad", b)
		if rec.Code < 400 || rec.Code >= 500 || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: PUT = %d %q, want a 4xx naming %q", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	if code := s.Do("GET", "/api/dashboards/bad", nil).Code; code != 404 {
		t.Errorf("a refused dashboard was kept: GET = %d", code)
	}
}

// A tile on a cron deleted after the dashboard was saved draws a placeholder:
// the dashboard still loads.
func dashboardMissingCron(t *testing.T, s Subject) {
	s.saveCron(t, "errors", "echo 1", cron.ValueNumber)
	if code := putDashboard(s, "ops", "tiles:\n  - {kind: counter, cron: ops/errors}\n"); code != 204 {
		t.Fatalf("PUT = %d", code)
	}
	if err := s.Crons.DeleteWatch(s.Owner, s.namespace(t, "ops"), "errors"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Crons map[string]any
	}
	rec := s.Do("GET", "/api/dashboards/ops", nil)
	if rec.Code != 200 {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	decode(t, rec, &got)
	if v, ok := got.Crons["ops/errors"]; !ok || v != nil {
		t.Errorf("crons = %+v, want ops/errors present and null", got.Crons)
	}
}
