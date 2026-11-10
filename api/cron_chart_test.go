package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// The shell's `chart <cron>` sends its range spec; the server resolves it
// and returns every tag's points inside the window, bounded at both ends.
func TestWatchChart_RangeSpec(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("cronchart-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("cronchart-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, "cc")
	jwt, _ := auth.Generate(user, "test-secret")

	if err := s.SaveCronWatch(cust.ID, opsNS(t, s, cust.ID), user.ID, "hosts", "every 5m", "echo 1 a", true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetCronWatch(cust.ID, opsNS(t, s, cust.ID), "hosts")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	val := func(v float64) *float64 { return &v }
	if err := s.SaveCronResults([]store.CronResult{
		{CronWatchID: w.ID, RanAt: now.Add(-3 * time.Hour), Value: val(9), Tag: "a"}, // before the window
		{CronWatchID: w.ID, RanAt: now.Add(-90 * time.Minute), Value: val(1), Tag: "a"},
		{CronWatchID: w.ID, RanAt: now.Add(-90 * time.Minute), Value: val(2), Tag: "b"},
		{CronWatchID: w.ID, RanAt: now.Add(-10 * time.Minute), Value: val(3), Tag: "a"}, // after the window
	}); err != nil {
		t.Fatal(err)
	}

	spec := "2h..30m"
	req := httptest.NewRequest("GET", "/api/crons/ops/hosts/chart?spec="+url.QueryEscape(spec), nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Granularity string `json:"granularity"`
		Start, End  int64
		Points      []struct {
			Tag   string  `json:"tag"`
			Count int64   `json:"count"`
			Sum   float64 `json:"sum"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Granularity != "raw" || len(resp.Points) != 2 {
		t.Fatalf("granularity=%q points=%+v, want the two raw points inside 2h..30m", resp.Granularity, resp.Points)
	}
	if resp.Points[0].Tag != "a" || resp.Points[0].Sum != 1 || resp.Points[1].Tag != "b" || resp.Points[1].Sum != 2 {
		t.Fatalf("points = %+v", resp.Points)
	}
	if got := time.UnixMilli(resp.End); now.Add(-30*time.Minute).Sub(got).Abs() > 2*time.Minute {
		t.Errorf("end = %s, want about 30m ago", got)
	}

	// A dashboard tile on the cron gets its plot: one line per stored tag.
	if err := s.SaveDashboard(cust.ID, user.ID, "fleet", "tiles:\n  - kind: sparkline\n    cron: ops/hosts\n"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/api/dashboards/fleet", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dashboard status %d: %s", rec.Code, rec.Body)
	}
	var dash struct {
		Spec struct {
			Sections []struct {
				Tiles []struct {
					Plot struct {
						Series []struct {
							Tag string `json:"tag"`
						} `json:"series"`
					} `json:"plot"`
				} `json:"tiles"`
			} `json:"sections"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if s := dash.Spec.Sections[0].Tiles[0].Plot.Series; len(s) != 2 || s[0].Tag != "a" || s[1].Tag != "b" {
		t.Fatalf("dashboard plot series = %+v, want lines a and b", s)
	}

	// Tiles on one cron reduce it separately: a line draws its own tags, and
	// a bar of a tagged cron says why it has no plot.
	if err := s.SaveDashboard(cust.ID, user.ID, "fleet2", "tiles:\n  - kind: line\n    cron: ops/hosts\n    aggregate: max\n    bucket: 1h\n  - kind: bar\n    cron: ops/hosts\n"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/api/dashboards/fleet2", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var dash2 struct {
		Spec struct {
			Sections []struct {
				Tiles []struct {
					Plot  *struct{ Kind string } `json:"plot"`
					Error string                 `json:"error"`
				} `json:"tiles"`
			} `json:"sections"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash2); err != nil {
		t.Fatal(err)
	}
	tiles := dash2.Spec.Sections[0].Tiles
	if tiles[0].Plot == nil || tiles[0].Plot.Kind != "line" || tiles[0].Error != "" {
		t.Errorf("line tile = %+v, want a line plot", tiles[0])
	}
	if tiles[1].Plot != nil || !strings.Contains(tiles[1].Error, "a bar chart draws one series") {
		t.Errorf("bar tile of a tagged cron = %+v, want the bar error and no plot", tiles[1])
	}

	// A tile's range: picks the window it draws, so one cron's tiles differ.
	if err := s.SaveDashboard(cust.ID, user.ID, "fleet3", "tiles:\n  - kind: line\n    cron: ops/hosts\n    range: 2h..30m\n  - kind: line\n    cron: ops/hosts\n    range: 5m..\n"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/api/dashboards/fleet3", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var dash3 struct {
		Spec struct {
			Sections []struct {
				Tiles []struct {
					Plot struct {
						Series []struct{} `json:"series"`
					} `json:"plot"`
				} `json:"tiles"`
			} `json:"sections"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash3); err != nil {
		t.Fatal(err)
	}
	if got := dash3.Spec.Sections[0].Tiles; len(got[0].Plot.Series) != 2 || len(got[1].Plot.Series) != 0 {
		t.Errorf("series by range = %d, %d; want the 2h..30m tile to draw both tags and the 5m.. tile none", len(got[0].Plot.Series), len(got[1].Plot.Series))
	}

	// A counter and a table with a range read an aggregate over it, per tag,
	// instead of the latest run. hosts stored a=1 and b=2 within 2h..30m.
	if err := s.SaveDashboard(cust.ID, user.ID, "fleet4", "tiles:\n  - kind: counter\n    cron: ops/hosts\n    aggregate: sum\n    range: 2h..30m\n  - kind: table\n    cron: ops/hosts\n    aggregate: max\n    range: 2h..30m\n  - kind: table\n    cron: ops/hosts\n    range: 5m..\n"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/api/dashboards/fleet4", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var dash4 struct {
		Spec struct {
			Sections []struct {
				Tiles []struct {
					Display struct{ Num string } `json:"display"`
					Points  []struct {
						Tag   string
						Value float64
					} `json:"points"`
				} `json:"tiles"`
			} `json:"sections"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash4); err != nil {
		t.Fatal(err)
	}
	t4 := dash4.Spec.Sections[0].Tiles
	if t4[0].Display.Num != "2" {
		t.Errorf("windowed counter shows %q, want the highest tag's sum, 2", t4[0].Display.Num)
	}
	if len(t4[1].Points) != 2 || t4[1].Points[0].Tag != "b" || t4[1].Points[0].Value != 2 || t4[1].Points[1].Tag != "a" {
		t.Errorf("windowed table points = %+v, want b=2 then a=1", t4[1].Points)
	}
	if len(t4[2].Points) != 0 {
		t.Errorf("a range with no runs in it has no rows, got %+v", t4[2].Points)
	}

	// A text cron has nothing to chart, and its latest run comes back as
	// text points, on /latest and in a dashboard's table tile.
	if err := s.SaveCronWatch(cust.ID, opsNS(t, s, cust.ID), user.ID, "holders", "every 5m", "# type: text\necho i-0abc lease", true, nil); err != nil {
		t.Fatal(err)
	}
	hw, _ := s.GetCronWatch(cust.ID, opsNS(t, s, cust.ID), "holders")
	holder := "i-0abc"
	if err := s.SaveCronResults([]store.CronResult{{CronWatchID: hw.ID, RanAt: now, TextValue: &holder, Tag: "lease"}}); err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/api/crons/ops/holders/chart?spec=1h.."); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no numbers to chart") {
		t.Fatalf("chart of a text cron: %d %s", rec.Code, rec.Body)
	}
	if rec := get("/api/crons/ops/holders/latest"); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `[{"tag":"lease","text":"i-0abc"}]` {
		t.Fatalf("latest of a text cron: %d %s", rec.Code, rec.Body)
	}
	if err := s.SaveDashboard(cust.ID, user.ID, "leases", "tiles:\n  - kind: table\n    cron: ops/holders\n"); err != nil {
		t.Fatal(err)
	}
	if rec := get("/api/dashboards/leases"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"points":[{"tag":"lease","text":"i-0abc"}]`) {
		t.Fatalf("dashboard latest: %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest("GET", "/api/crons/ops/hosts/chart?spec=nonsense", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad spec: status %d, want 400", rec.Code)
	}
}

// opsNS is the id of namespace "ops", where these tests keep their crons: a
// cron is referenced as <namespace>/<name>.
func opsNS(t *testing.T, s *store.Store, org store.OrgID) uint {
	t.Helper()
	ns, err := s.FindOrCreateNamespace(org, "ops")
	if err != nil {
		t.Fatal(err)
	}
	return ns.ID
}
