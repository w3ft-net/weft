package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/w3ft-net/weft/backends/cron"
	dashstore "github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/dashboard"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/timerange"
)

// SetDashboards is where dashboards live. Without it the api has no dashboard
// routes. The shell's /etc/dashboards is the mounts' own, over the same store
// (see mounts.Dashboards): writes go through it, so a dashboard is checked the
// same way whoever saves it.
func (s *Server) SetDashboards(st dashstore.Store) { s.dashboards = st }

// dashboardCron is a cron's metadata, so a dashboard render needs no follow-up
// requests. What each tile shows rides on the tile, since tiles on one cron can
// differ in range, aggregate and bucket. Latest, the cron's latest run in table
// order, is the default for a tile that doesn't aggregate over a range.
type dashboardCron struct {
	Cron   cronJSON            `json:"cron"`
	Latest []chart.LatestPoint `json:"-"`
	watch  *cron.Watch
}

// dashboardGetResponse is the wire shape for GET /api/dashboards/{name}. Spec is
// the parsed body; Crons keys by the cron named in each tile, so the client
// renders by lookup. YAMLBody echoes the stored text, the authoritative source.
type dashboardGetResponse struct {
	Name      string                    `json:"name"`
	UpdatedAt string                    `json:"updated_at,omitempty"`
	Spec      *dashboard.Spec           `json:"spec"`
	Crons     map[string]*dashboardCron `json:"crons"`
	YAMLBody  string                    `json:"yaml_body"`
}

// dashboardListItem is one entry of GET /api/dashboards. Title comes from the
// parsed YAML when it parses; otherwise the sidebar shows the name, so a broken
// dashboard can still be opened and fixed.
type dashboardListItem struct {
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Invalid   bool   `json:"invalid,omitempty"`
}

const dashboardTimeFmt = "2006-01-02T15:04:05Z07:00"

func (s *Server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if !s.requireGroup(w, session) {
		return
	}
	list, err := s.dashboards.List(session.Principal().Partition())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	items := make([]dashboardListItem, len(list))
	for i, d := range list {
		items[i] = dashboardListItem{Name: d.Name, UpdatedAt: d.UpdatedAt.Format(dashboardTimeFmt)}
		if spec, err := dashboard.Parse(d.YAMLBody); err == nil {
			items[i].Title = spec.Title
		} else {
			items[i].Invalid = true
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"dashboards": items})
}

func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if !s.requireGroup(w, session) {
		return
	}
	owner := session.Principal().Partition()
	d, err := s.dashboards.Get(owner, name)
	if err != nil {
		if errors.Is(err, dashstore.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	spec, err := dashboard.Parse(d.YAMLBody)
	if err != nil {
		http.Error(w, "dashboard yaml is invalid: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	readable, err := s.readable(r.Context(), session)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	crons := map[string]*dashboardCron{}
	restricted := map[string]bool{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if _, seen := crons[tile.Watch]; seen || restricted[tile.Watch] {
				continue
			}
			wt, err := s.cronNamed(owner, tile.Watch)
			if err == nil && !slices.Contains(readable, strings.SplitN(tile.Watch, "/", 2)[0]) {
				// A cron the viewer can't read: its tile stays, with the title the
				// dashboard's author wrote, and shows nothing else.
				restricted[tile.Watch] = true
				continue
			}
			if err != nil {
				// A tile on a cron that no longer exists gets an empty payload,
				// so the client draws a placeholder instead of failing the load.
				crons[tile.Watch] = nil
				continue
			}
			latest, err := s.latestPoints(wt.ID)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			crons[tile.Watch] = &dashboardCron{Cron: cronOf(*wt), Latest: latest, watch: wt}
		}
	}

	// A series tile draws the cron over the tile's own range, so tiles on one cron
	// share a read only when they share a range.
	type seriesKey struct{ cron, rng string }
	seriesByTile := map[seriesKey]chart.CronSeries{}
	limits := s.limitsOf(session)
	readSeries := func(tile dashboard.Tile, wt *cron.Watch) (chart.CronSeries, error) {
		key := seriesKey{tile.Watch, tile.TileRange()}
		if series, ok := seriesByTile[key]; ok {
			return series, nil
		}
		start, end, err := timerange.ParseRange(key.rng) // validated when the file was saved
		if err != nil {
			return chart.CronSeries{}, err
		}
		series, err := s.crons.Series(wt.ID, limits.Clamp(start), end)
		if err != nil {
			return chart.CronSeries{}, err
		}
		cs := chart.CronSeries{Granularity: series.Granularity, Start: series.Start, End: series.End,
			Buckets: make([]chart.CronBucket, len(series.Buckets)), Alert: cronAlert(wt)}
		for i, b := range series.Buckets {
			cs.Buckets[i] = chart.CronBucket{Start: b.Start, Tag: b.Tag, Count: b.Count, Sum: b.Sum, Min: b.Min, Max: b.Max}
		}
		seriesByTile[key] = cs
		return cs, nil
	}

	// Fill each tile once; clients render what the server computed. A series tile
	// gets its plot, a counter, gauge or table its points (the cron's latest run,
	// or its aggregate over the tile's range), a counter or gauge its formatted
	// text. Points are in table order, so on a tagged cron a counter shows its
	// highest value.
	for si := range spec.Sections {
		tiles := spec.Sections[si].Tiles
		for ti := range tiles {
			tile := &tiles[ti]
			if restricted[tile.Watch] {
				tile.Restricted, tile.Watch = true, ""
				continue
			}
			dc := crons[tile.Watch]
			if dc == nil {
				tile.Display = dashboard.FormatTileDisplay(*tile, nil)
				continue
			}
			alert := cronAlert(dc.watch)
			var shown []chart.LatestPoint // what the tile's headline number comes from
			switch {
			case tile.DrawsSeries():
				shown = dc.Latest
				series, err := readSeries(*tile, dc.watch)
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
				series, err := readSeries(*tile, dc.watch)
				if err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				var msg string
				shown, msg = chart.CronTotals(series, tile.ChartArgs().Aggregate)
				tile.Error = msg
			default:
				shown = dc.Latest
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
			tile.Display = dashboard.FormatTileDisplay(*tile, headline)
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
		UpdatedAt: d.UpdatedAt.Format(dashboardTimeFmt),
		Spec:      spec,
		Crons:     crons,
		YAMLBody:  d.YAMLBody,
	}
	if len(restricted) > 0 {
		// The source names the crons the viewer may not read.
		resp.YAMLBody = ""
	}
	json.NewEncoder(w).Encode(resp)
}

// cronNamed is the cron a tile names, <namespace>/<name> or an org-level cron's
// bare name, whether or not the caller may read it: the dashboard says which of
// a missing and an unreadable one it is.
func (s *Server) cronNamed(owner shape.Added, p string) (*cron.Watch, error) {
	var ns uint
	name := p
	if home, rest, homed := strings.Cut(p, "/"); homed {
		if rest == "" || strings.Contains(rest, "/") {
			return nil, cron.ErrNotFound
		}
		n, ok, err := s.sources.GetNamespace(owner, home)
		if err != nil || !ok {
			return nil, cron.ErrNotFound
		}
		ns, name = n.ID, rest
	}
	return s.crons.GetWatch(owner, ns, name)
}

// handleSaveDashboard implements PUT /api/dashboards/{name}; the body is
// {"yaml_body": "..."}. The /etc mount checks the grammar, the crons it names and
// who may write.
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
	if !s.requireGroup(w, session) {
		return
	}
	fp, ok := dashboardPath(name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).WriteFile(r.Context(), fp, []byte(req.YAMLBody)); err != nil {
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
	if !s.requireGroup(w, session) {
		return
	}
	fp, ok := dashboardPath(name)
	if !ok {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if err := s.shellFS(r, session, nil).Remove(r.Context(), fp); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dashboardPath is /etc/dashboards/<name>.yaml when name is a file name that
// stays below the directory.
func dashboardPath(name string) (string, bool) {
	p := "/etc/dashboards/" + name + ".yaml"
	return p, path.Clean(p) == p
}
