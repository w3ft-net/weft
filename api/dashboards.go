package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/graybeardcorp/weft/store"
	"gorm.io/gorm"
)

// dashboardWatchPayload bundles a watch's metadata and its recent
// results so a dashboard render needs no follow-up requests. Mirrors
// the shape that handleWatchResults returns for the single-watch view.
type dashboardWatchPayload struct {
	Watch   *store.CronWatch   `json:"watch"`
	Results []store.CronResult `json:"results"`
}

// dashboardGetResponse is the wire shape for GET /api/dashboards/{name}.
// Spec is the parsed/validated body; Watches keys by the cron-watch name
// referenced from each tile, so the client renders by lookup. YAMLBody
// echoes the on-disk text so HTTPBackend (and any vi-edit roundtrip)
// has the authoritative source.
type dashboardGetResponse struct {
	Name      string                            `json:"name"`
	UpdatedAt string                            `json:"updated_at,omitempty"`
	Spec      *store.DashboardSpec              `json:"spec"`
	Watches   map[string]*dashboardWatchPayload `json:"watches"`
	YAMLBody  string                            `json:"yaml_body"`
}

// dashboardListItem is one entry of GET /api/dashboards. Title comes
// from the parsed YAML when it parses; otherwise we fall back to the
// store name so the sidebar still has something to show for a broken
// dashboard the user can edit and fix.
type dashboardListItem struct {
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Invalid   bool   `json:"invalid,omitempty"`
}

func (s *Server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, err := s.resolveCustomerID(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dashboards, err := s.store.ListDashboards(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	items := make([]dashboardListItem, len(dashboards))
	for i, d := range dashboards {
		item := dashboardListItem{
			Name:      d.Name,
			UpdatedAt: d.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if spec, err := store.ParseDashboardYAML(d.YAMLBody); err == nil {
			item.Title = spec.Title
		} else {
			item.Invalid = true
		}
		items[i] = item
	}
	json.NewEncoder(w).Encode(map[string]any{
		"dashboards": items,
	})
}

func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d, err := s.store.GetDashboard(custID, name)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	spec, err := store.ParseDashboardYAML(d.YAMLBody)
	if err != nil {
		http.Error(w, "dashboard yaml is invalid: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	watches := map[string]*dashboardWatchPayload{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if _, seen := watches[tile.Watch]; seen {
				continue
			}
			cw, err := s.store.GetCronWatch(custID, tile.Watch)
			if err != nil {
				// Tile references a watch that no longer exists — emit
				// an empty payload so the client can render a placeholder
				// rather than fail the whole dashboard load.
				watches[tile.Watch] = nil
				continue
			}
			results, err := s.store.ListCronResults(cw.ID, 50, "")
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			watches[tile.Watch] = &dashboardWatchPayload{
				Watch:   cw,
				Results: results,
			}
		}
	}

	resp := dashboardGetResponse{
		Name:      d.Name,
		UpdatedAt: d.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Spec:      spec,
		Watches:   watches,
		YAMLBody:  d.YAMLBody,
	}
	json.NewEncoder(w).Encode(resp)
}

// handleSaveDashboard implements PUT /api/dashboards/{name}. Body is
// {"yaml_body": "..."}. Validates the YAML grammar and that every
// referenced watch exists for this customer before persisting.
func (s *Server) handleSaveDashboard(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	var req struct {
		YAMLBody string `json:"yaml_body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.YAMLBody == "" {
		http.Error(w, "yaml_body required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	spec, err := store.ParseDashboardYAML(req.YAMLBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.ValidateDashboardWatchRefs(custID, spec); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.SaveDashboard(custID, session.UserID, name, req.YAMLBody); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteDashboard(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.DeleteDashboard(custID, name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
