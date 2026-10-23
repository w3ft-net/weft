package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/store"
	"gorm.io/gorm"
)

// dashboardWatchPayload bundles a cron's metadata, its latest run (table,
// counter and gauge tiles) and its plot over dashboardWindow (sparkline
// tiles), so a dashboard render needs no follow-up requests. The latest
// run is one point per tag in table order; the plot is reduced like
// `chart <cron>`: one line per stored tag, the rest folded into "other".
type dashboardWatchPayload struct {
	Watch  *store.CronWatch    `json:"cron"`
	Latest []chart.LatestPoint `json:"latest"`
	Plot   *chart.Plot         `json:"plot"`
}

// dashboardWindow is how far back a dashboard's sparklines reach.
const dashboardWindow = 24 * time.Hour

// cronPlot reads a cron's stored series over [start, end) and reduces it
// to the plot the chart renderers draw.
func (s *Server) cronPlot(orgID store.OrgID, cronWatchID uint, start, end time.Time) (*chart.Plot, error) {
	series, err := s.store.CronSeries(orgID, cronWatchID, start, end)
	if err != nil {
		return nil, err
	}
	cs := chart.CronSeries{Granularity: series.Granularity, Start: series.Start, End: series.End, Buckets: make([]chart.CronBucket, len(series.Buckets))}
	for i, b := range series.Buckets {
		cs.Buckets[i] = chart.CronBucket{Start: b.BucketStart, Tag: b.Tag, Count: b.Count, Sum: b.Sum, Min: b.Min, Max: b.Max}
	}
	plot, msg := chart.CronPlot(cs, chart.WatchArgs{Kind: "line", Aggregate: "avg"})
	if msg != "" {
		return nil, errors.New(msg)
	}
	return &plot, nil
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
	Watches   map[string]*dashboardWatchPayload `json:"crons"`
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
	custID, ok := activeOrg(w, session)
	if !ok {
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
	custID, ok := activeOrg(w, session)
	if !ok {
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
			latest, err := s.latestPoints(cw.ID)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			now := time.Now()
			plot, err := s.cronPlot(custID, cw.ID, now.Add(-dashboardWindow), now)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			watches[tile.Watch] = &dashboardWatchPayload{
				Watch:  cw,
				Latest: latest,
				Plot:   plot,
			}
		}
	}

	// Format counter/gauge text here, once; clients render tile.display.
	// The latest run is in table order, so on a tagged cron a counter
	// shows its highest value.
	for si := range spec.Sections {
		tiles := spec.Sections[si].Tiles
		for ti := range tiles {
			var latest *float64
			if wp := watches[tiles[ti].Watch]; wp != nil {
				for _, p := range wp.Latest {
					if p.Value != nil {
						latest = p.Value
						break
					}
				}
			}
			tiles[ti].Display = store.FormatTileDisplay(tiles[ti], latest)
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
	custID, ok := activeOrg(w, session)
	if !ok {
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
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	if err := s.store.DeleteDashboard(custID, name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
