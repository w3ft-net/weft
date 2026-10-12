package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// dashTeamMember registers `user` as an owner of `cust` so the
// auth middleware's BuildSession finds a membership and resolves
// the active org. Without this the dashboards-handler chain hits
// ErrNoMembership and returns 403.
func dashTeamMember(s *store.Store, cust *store.Organization, user *store.User, idSuffix string) {
	_ = s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
}

func TestDashboards_ListAndGet(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("dashapi-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("dash-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, fmt.Sprintf("la-%d", os.Getpid()))
	jwt, _ := auth.Generate(user, "test-secret")

	// Seed a watch and a dashboard that references it.
	if err := s.SaveCronWatch(cust.ID, user.ID, "error-rate-hourly", "every 5m", "echo 1", nil); err != nil {
		t.Fatalf("seed watch: %v", err)
	}
	dashYAML := `title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: error-rate-hourly
        size: md
`
	if err := s.SaveDashboard(cust.ID, user.ID, "prod", dashYAML); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}

	// List.
	req := httptest.NewRequest("GET", "/api/dashboards", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Dashboards []dashboardListItem `json:"dashboards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Dashboards) != 1 {
		t.Fatalf("want 1 dashboard, got %d", len(listResp.Dashboards))
	}
	if listResp.Dashboards[0].Name != "prod" {
		t.Errorf("name: got %q", listResp.Dashboards[0].Name)
	}
	if listResp.Dashboards[0].Title != "Production" {
		t.Errorf("title: got %q", listResp.Dashboards[0].Title)
	}

	// Get.
	req = httptest.NewRequest("GET", "/api/dashboards/prod", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var getResp dashboardGetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if getResp.Spec == nil || getResp.Spec.Title != "Production" {
		t.Fatalf("spec missing or wrong title: %+v", getResp.Spec)
	}
	if len(getResp.Spec.Sections) != 1 || len(getResp.Spec.Sections[0].Tiles) != 1 {
		t.Fatalf("unexpected sections shape: %+v", getResp.Spec.Sections)
	}
	wp, ok := getResp.Watches["error-rate-hourly"]
	if !ok {
		t.Fatalf("watches missing key, got: %+v", getResp.Watches)
	}
	if wp == nil || wp.Watch == nil || wp.Watch.Name != "error-rate-hourly" {
		t.Errorf("watch payload: %+v", wp)
	}
}

func TestDashboards_Get_NotFound(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("dashapi-nf-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("dash-nf-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, fmt.Sprintf("nf-%d", os.Getpid()))
	jwt, _ := auth.Generate(user, "test-secret")

	req := httptest.NewRequest("GET", "/api/dashboards/nope", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDashboards_List_Empty(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("dashapi-empty-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("dash-empty-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, fmt.Sprintf("e-%d", os.Getpid()))
	jwt, _ := auth.Generate(user, "test-secret")

	req := httptest.NewRequest("GET", "/api/dashboards", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	var resp struct {
		Dashboards []dashboardListItem `json:"dashboards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Dashboards) != 0 {
		t.Errorf("expected 0 dashboards, got %d", len(resp.Dashboards))
	}
}

// The server formats counter and gauge text once (store.FormatTileDisplay);
// the web client renders tile.display verbatim and never formats numbers.
func TestDashboards_Get_TileDisplay(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("dashdisp-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("dashdisp-%d@example.com", os.Getpid()))
	dashTeamMember(s, cust, user, "disp")
	jwt, _ := auth.Generate(user, "test-secret")

	seed := func(name string, v *float64) {
		if err := s.SaveCronWatch(cust.ID, user.ID, name, "every 5m", "echo 1", nil); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if v == nil {
			return
		}
		cw, err := s.GetCronWatch(cust.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveCronResults([]store.CronResult{{CronWatchID: cw.ID, RanAt: time.Now(), Value: v}}); err != nil {
			t.Fatalf("seed result %s: %v", name, err)
		}
	}
	f := func(v float64) *float64 { return &v }
	seed("lat", f(41.6))
	seed("plain", f(3.5))
	seed("load", f(50))
	seed("empty", nil)

	yaml := `tiles:
  - {kind: counter, cron: lat, format: "%d ms"}
  - {kind: counter, cron: plain}
  - {kind: gauge, cron: load, threshold: {max: 200}}
  - {kind: counter, cron: empty}
  - {kind: sparkline, cron: plain}
`
	if err := s.SaveDashboard(cust.ID, user.ID, "disp", yaml); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/dashboards/disp", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var resp dashboardGetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tiles := resp.Spec.Sections[0].Tiles
	want := []*store.TileDisplay{
		{Num: "42", After: " ms"},
		{Num: "3.50"},
		{Num: "50", After: "/200"},
		{Num: "—"},
		nil,
	}
	for i, wd := range want {
		got := tiles[i].Display
		if (wd == nil) != (got == nil) || (wd != nil && *wd != *got) {
			t.Errorf("tile %d display = %+v, want %+v", i, got, wd)
		}
	}
}
