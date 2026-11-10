package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/internal/timerange"
	"github.com/graybeardcorp/weft/store"
	"gorm.io/gorm"
)

// dashboardWatchPayload bundles a cron's metadata so a dashboard render
// needs no follow-up requests. What each tile shows rides on the tile, since
// tiles on one cron can differ in range, aggregate and bucket. Latest, the
// cron's latest run in table order, is the default for a tile that doesn't
// aggregate over a range.
type dashboardWatchPayload struct {
	Watch  *store.CronWatch    `json:"cron"`
	Latest []chart.LatestPoint `json:"-"`
}

// cronSeries reads a cron's stored series over [start, end).
func (s *Server) cronSeries(orgID store.OrgID, cw *store.CronWatch, start, end time.Time) (chart.CronSeries, error) {
	series, err := s.store.CronSeries(orgID, cw.ID, start, end)
	if err != nil {
		return chart.CronSeries{}, err
	}
	cs := chart.CronSeries{Granularity: series.Granularity, Start: series.Start, End: series.End, Buckets: make([]chart.CronBucket, len(series.Buckets)), Alert: cw.Alert()}
	for i, b := range series.Buckets {
		cs.Buckets[i] = chart.CronBucket{Start: b.BucketStart, Tag: b.Tag, Count: b.Count, Sum: b.Sum, Min: b.Min, Max: b.Max}
	}
	return cs, nil
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
	// A series tile draws its cron over the tile's own range, so tiles on one
	// cron share a read only when they share a range.
	type seriesKey struct{ cron, rng string }
	seriesByTile := map[seriesKey]chart.CronSeries{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if _, seen := watches[tile.Watch]; seen {
				continue
			}
			cw, err := s.store.GetCronWatch(custID, 0, tile.Watch)
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
			watches[tile.Watch] = &dashboardWatchPayload{
				Watch:  cw,
				Latest: latest,
			}
		}
	}

	// Fill each tile once; clients render what the server computed. A series
	// tile gets its plot, a counter, gauge or table its points (the cron's
	// latest run, or its aggregate over the tile's range), a counter or
	// gauge its formatted text. Points are in table order, so on a tagged
	// cron a counter shows its highest value.
	readSeries := func(tile store.DashboardTile, cw *store.CronWatch) (chart.CronSeries, error) {
		key := seriesKey{tile.Watch, tile.TileRange()}
		if series, ok := seriesByTile[key]; ok {
			return series, nil
		}
		start, end, err := timerange.ParseRange(key.rng) // validated when the file was saved
		if err != nil {
			return chart.CronSeries{}, err
		}
		series, err := s.cronSeries(custID, cw, start, end)
		seriesByTile[key] = series
		return series, err
	}
	for si := range spec.Sections {
		tiles := spec.Sections[si].Tiles
		for ti := range tiles {
			tile := &tiles[ti]
			wp := watches[tile.Watch]
			if wp == nil {
				tile.Display = store.FormatTileDisplay(*tile, nil)
				continue
			}
			alert := wp.Watch.Alert()
			var shown []chart.LatestPoint // what the tile's headline number comes from
			switch {
			case tile.DrawsSeries():
				shown = wp.Latest
				series, err := readSeries(*tile, wp.Watch)
				if err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				plot, msg := chart.CronPlot(series, tile.ChartArgs())
				if msg == "" && plot.Kind == "bar" && len(plot.Series) > 1 {
					msg = chart.BarTaggedMsg(tile.Watch, len(plot.Series))
				}
				if msg != "" {
					tile.Error = msg
				} else {
					tile.Plot = &plot
				}
			case tile.Windowed():
				series, err := readSeries(*tile, wp.Watch)
				if err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				var msg string
				shown, msg = chart.CronTotals(series, tile.ChartArgs().Aggregate)
				tile.Error = msg
			default:
				shown = wp.Latest
			}
			var headline *float64
			for i := range shown {
				if shown[i].Value != nil {
					if headline == nil {
						headline = shown[i].Value
					}
					shown[i].Critical = alert.Breached(*shown[i].Value)
				}
			}
			tile.Display = store.FormatTileDisplay(*tile, headline)
			if tile.Kind != "table" && tile.Kind != "bar" && headline != nil {
				tile.Critical = alert.Breached(*headline)
			}
			if tile.Kind == "counter" || tile.Kind == "gauge" || tile.Kind == "table" {
				tile.Points = shown
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
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	p, ok := mountPath("/etc/dashboards", name+".yaml")
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	// The /etc mount validates the YAML and its cron references.
	if err := s.shellFS(r, session, nil).WriteFile(p, []byte(req.YAMLBody)); err != nil {
		writeFSError(w, err)
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
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	p, ok := mountPath("/etc/dashboards", name+".yaml")
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).Remove(p); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
