package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
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

	if err := s.SaveCronWatch(cust.ID, user.ID, "hosts", "every 5m", "echo 1 a", true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetCronWatch(cust.ID, "hosts")
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
	req := httptest.NewRequest("GET", "/api/crons/hosts/chart?spec="+url.QueryEscape(spec), nil)
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
	if err := s.SaveDashboard(cust.ID, user.ID, "fleet", "tiles:\n  - kind: sparkline\n    cron: hosts\n"); err != nil {
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
		Crons map[string]struct {
			Plot struct {
				Series []struct {
					Tag string `json:"tag"`
				} `json:"series"`
			} `json:"plot"`
		} `json:"crons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if s := dash.Crons["hosts"].Plot.Series; len(s) != 2 || s[0].Tag != "a" || s[1].Tag != "b" {
		t.Fatalf("dashboard plot series = %+v, want lines a and b", s)
	}

	req = httptest.NewRequest("GET", "/api/crons/hosts/chart?spec=nonsense", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad spec: status %d, want 400", rec.Code)
	}
}
