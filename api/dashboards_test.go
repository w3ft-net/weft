package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// dashTeamMember registers `user` as an owner of `cust` so the
// auth middleware's BuildSession finds a membership and resolves
// the active org. Without this the dashboards-handler chain hits
// ErrNoMembership and returns 403.
func dashTeamMember(s *store.Store, cust *store.Customer, user *store.User, idSuffix string) {
	_ = s.CreateTeamMember(&store.TeamMember{
		ID: "tm-dash-" + idSuffix, CustomerID: cust.ID, Email: user.Email,
		UserID: &user.ID, Role: "owner", Status: "active",
	})
}

func TestDashboards_ListAndGet(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("dashapi-%d", os.Getpid()))
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
        watch: error-rate-hourly
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

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("dashapi-nf-%d", os.Getpid()))
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

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("dashapi-empty-%d", os.Getpid()))
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
