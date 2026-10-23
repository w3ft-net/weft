package store

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// Dashboard is a customer-scoped named dashboard. The YAML body is the
// authoritative form — exactly what the user wrote into
// /etc/dashboards/<name>.yaml. The shell reads/writes it as a file; the
// API parses it on read via ParseDashboardYAML to return a structured
// spec to the frontend.
type Dashboard struct {
	gorm.Model
	OrgID           OrgID   `gorm:"not null;uniqueIndex:idx_dashboards_customer_name"`
	Name            string  `gorm:"not null;uniqueIndex:idx_dashboards_customer_name"`
	YAMLBody        string  `gorm:"column:yaml_body;type:text;not null"`
	CreatedByUserID *string `gorm:"column:created_by_user_id"`
}

// DashboardSpec is the parsed form of a dashboard YAML body. The shell
// and the API share this representation. A flat top-level `tiles:` list
// is normalized into a single untitled section.
type DashboardSpec struct {
	Title    string             `yaml:"title,omitempty" json:"title,omitempty"`
	Sections []DashboardSection `yaml:"sections,omitempty" json:"sections"`
}

// DashboardSection groups tiles under an optional heading.
type DashboardSection struct {
	Title string          `yaml:"title,omitempty" json:"title,omitempty"`
	Tiles []DashboardTile `yaml:"tiles" json:"tiles"`
}

// DashboardTile is one tile entry. Watch is the cron watch name to read
// from. Size is one of "sm", "md", "lg" (default "md"). Kind-specific
// fields (Format, Threshold, Columns) are optional.
//
// Tag and Y are sparkline-only: when Tag is set, the watch's stdout is
// treated as multiple JSON rows per run (e.g. one per coordinator
// instance) instead of a single scalar. Tag is the JSON path that
// identifies which entity a row belongs to; Y is the JSON path to the
// value to plot. Each row's run timestamp (not a per-row field) is the
// x value, since a dashboard tile's history comes from the cron's
// run history, not a live pipeline. The tile then renders one
// line per distinct Tag value instead of one line for the whole cron.
type DashboardTile struct {
	Kind      string           `yaml:"kind" json:"kind"`
	Watch     string           `yaml:"cron" json:"cron"`
	Title     string           `yaml:"title,omitempty" json:"title,omitempty"`
	Size      string           `yaml:"size,omitempty" json:"size,omitempty"`
	Format    string           `yaml:"format,omitempty" json:"format,omitempty"`
	Threshold *DashboardThresh `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	Columns   []string         `yaml:"columns,omitempty" json:"columns,omitempty"`

	// Display is filled by the API when serving a dashboard; never read from YAML.
	Display *TileDisplay `yaml:"-" json:"display,omitempty"`
}

// DashboardThresh is the optional warn/crit cutoff for counter and gauge tiles.
type DashboardThresh struct {
	Warn *float64 `yaml:"warn,omitempty" json:"warn,omitempty"`
	Crit *float64 `yaml:"crit,omitempty" json:"crit,omitempty"`
	Max  *float64 `yaml:"max,omitempty" json:"max,omitempty"` // gauge upper bound
}

// dashboardYAMLRaw is the on-disk shape — accepts either `sections:` or
// the shorthand top-level `tiles:`.
type dashboardYAMLRaw struct {
	Title    string             `yaml:"title"`
	Sections []DashboardSection `yaml:"sections"`
	Tiles    []DashboardTile    `yaml:"tiles"`
}

var (
	validTileKinds = map[string]bool{
		"sparkline": true,
		"counter":   true,
		"gauge":     true,
		"table":     true,
	}
	validTileSizes = map[string]bool{
		"sm": true,
		"md": true,
		"lg": true,
	}
)

// tileFormatToken matches the pieces of a counter `format:` string: the
// literal-percent escape %% and the two value verbs, %d (rounded integer)
// and %.Nf (N decimals). Any other % is invalid.
var tileFormatToken = regexp.MustCompile(`%%|%d|%\.\d+f`)

// ValidateTileFormat reports whether f is a usable counter format: literal
// text plus exactly one value verb (%d or %.Nf), with %% for a literal %.
func ValidateTileFormat(f string) error {
	verbs := 0
	for _, m := range tileFormatToken.FindAllString(f, -1) {
		if m != "%%" {
			verbs++
		}
	}
	if verbs != 1 || strings.Contains(tileFormatToken.ReplaceAllString(f, ""), "%") {
		return fmt.Errorf("`format:` must contain exactly one %%d or %%.Nf (e.g. \"%%d ms\", \"%%.2f%%%%\"), got %q", f)
	}
	return nil
}

// TileDisplay is the server-formatted text of a counter or gauge tile:
// Num is the headline number, Before/After the literal text around it
// (a counter's format, a gauge's "/max"). Every client — the terminal
// renderer and the web UI — shows these strings verbatim; none formats
// numbers itself, so there is exactly one formatter.
type TileDisplay struct {
	Before string `json:"before,omitempty"`
	Num    string `json:"num"`
	After  string `json:"after,omitempty"`
}

// FormatNumber is the default number text: an integer when v is whole,
// else two decimals.
func FormatNumber(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// FormatTileDisplay formats the latest value v (nil: no data) for a
// counter or gauge tile and returns nil for every other kind. The
// tile's format must already have passed ValidateTileFormat.
func FormatTileDisplay(t DashboardTile, v *float64) *TileDisplay {
	switch t.Kind {
	case "counter":
		if v == nil {
			return &TileDisplay{Num: "—"}
		}
		if t.Format == "" {
			return &TileDisplay{Num: FormatNumber(*v)}
		}
		var before, after strings.Builder
		cur, num, last := &before, "", 0
		for _, loc := range tileFormatToken.FindAllStringIndex(t.Format, -1) {
			cur.WriteString(t.Format[last:loc[0]])
			last = loc[1]
			switch tok := t.Format[loc[0]:loc[1]]; tok {
			case "%%":
				cur.WriteString("%")
			case "%d":
				num, cur = fmt.Sprintf("%d", int64(math.Round(*v))), &after
			default:
				num, cur = fmt.Sprintf(tok, *v), &after
			}
		}
		cur.WriteString(t.Format[last:])
		return &TileDisplay{Before: before.String(), Num: num, After: after.String()}
	case "gauge":
		max := 100.0
		if t.Threshold != nil && t.Threshold.Max != nil {
			max = *t.Threshold.Max
		}
		num := "—"
		if v != nil {
			num = FormatNumber(*v)
		}
		return &TileDisplay{Num: num, After: "/" + FormatNumber(max)}
	}
	return nil
}

// knownKeys lists the valid YAML keys at each nesting level so
// friendlyYAMLError can suggest what the user probably meant.
var knownKeys = map[string][]string{
	"dashboardYAMLRaw": {"title", "sections", "tiles"},
	"DashboardSection": {"title", "tiles"},
	"DashboardTile":    {"kind", "cron", "title", "size", "format", "threshold", "columns"},
	"DashboardThresh":  {"warn", "crit", "max"},
}

// friendlyYAMLError rewrites the opaque yaml.v3 "field X not found in
// type store.Y" message into something actionable.
func friendlyYAMLError(err error) error {
	msg := err.Error()
	// yaml.v3 KnownFields errors look like:
	//   "line N: field FOO not found in type store.BAR"
	// or multiple of those joined in a yaml.TypeError.
	idx := strings.Index(msg, "field ")
	typeIdx := strings.Index(msg, "not found in type store.")
	if idx < 0 || typeIdx < 0 {
		return fmt.Errorf("yaml: %w", err)
	}
	field := strings.TrimSpace(msg[idx+len("field ") : typeIdx])
	typeName := msg[typeIdx+len("not found in type store."):]
	// Strip any trailing context (e.g. from multi-error joins).
	if i := strings.IndexByte(typeName, '\n'); i >= 0 {
		typeName = typeName[:i]
	}
	typeName = strings.TrimSpace(typeName)

	if typeName == "DashboardTile" && field == "watch" {
		return fmt.Errorf("saw 'watch', expected 'cron': write `cron: <name>` — run `man dashboard` for the YAML grammar")
	}
	if typeName == "DashboardTile" && (field == "tag" || field == "y") {
		return fmt.Errorf("a tile has no '%s' key: a sparkline draws one line per tag its cron stores. Declare the value and tag on the cron itself, with '# value:' and '# tag:' headers (JSON output) or alert -v/-t — run `help cron`", field)
	}
	valid := knownKeys[typeName]
	if len(valid) == 0 {
		return fmt.Errorf("'%s' is not a valid key — run `man dashboard` for the YAML grammar", field)
	}
	return fmt.Errorf("'%s' is not a valid key (expected one of: %s) — run `man dashboard` for the YAML grammar",
		field, strings.Join(valid, ", "))
}

// ParseDashboardYAML parses and grammar-validates a dashboard body.
// Returns a normalized DashboardSpec — flat `tiles:` becomes a single
// untitled section. Does not check that referenced watches exist;
// callers that need that should follow up with ValidateWatchRefs.
func ParseDashboardYAML(body string) (*DashboardSpec, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("dashboard is empty")
	}
	var raw dashboardYAMLRaw
	dec := yaml.NewDecoder(strings.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, friendlyYAMLError(err)
	}

	if len(raw.Sections) > 0 && len(raw.Tiles) > 0 {
		return nil, errors.New("dashboard cannot mix top-level `tiles:` with `sections:` — pick one")
	}

	spec := &DashboardSpec{Title: raw.Title}
	if len(raw.Tiles) > 0 {
		spec.Sections = []DashboardSection{{Tiles: raw.Tiles}}
	} else {
		spec.Sections = raw.Sections
	}

	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

func validateSpec(spec *DashboardSpec) error {
	if len(spec.Sections) == 0 {
		return errors.New("dashboard has no tiles")
	}
	tileCount := 0
	for si, sec := range spec.Sections {
		if len(sec.Tiles) == 0 {
			return fmt.Errorf("section %d (%q) has no tiles", si, sec.Title)
		}
		for ti, tile := range sec.Tiles {
			where := fmt.Sprintf("section %d tile %d", si, ti)
			if sec.Title != "" {
				where = fmt.Sprintf("section %q tile %d", sec.Title, ti)
			}
			if tile.Kind == "" {
				return fmt.Errorf("%s: missing `kind:`", where)
			}
			if !validTileKinds[tile.Kind] {
				return fmt.Errorf("%s: unknown kind %q (valid: sparkline, counter, gauge, table)", where, tile.Kind)
			}
			if tile.Watch == "" {
				return fmt.Errorf("%s: missing `cron:`", where)
			}
			if tile.Size != "" && !validTileSizes[tile.Size] {
				return fmt.Errorf("%s: unknown size %q (valid: sm, md, lg)", where, tile.Size)
			}
			if tile.Kind == "table" && len(tile.Columns) == 0 {
				return fmt.Errorf("%s: table tile requires `columns:`", where)
			}
			if tile.Format != "" {
				if tile.Kind != "counter" {
					return fmt.Errorf("%s: `format:` is only supported on counter tiles", where)
				}
				if err := ValidateTileFormat(tile.Format); err != nil {
					return fmt.Errorf("%s: %w", where, err)
				}
			}
			if tile.Threshold != nil && tile.Kind == "table" {
				return fmt.Errorf("%s: `threshold:` is only supported on counter, gauge, sparkline tiles", where)
			}
			if tile.Threshold != nil && tile.Threshold.Max != nil && tile.Kind != "gauge" {
				return fmt.Errorf("%s: `threshold.max:` is only supported on gauge tiles", where)
			}
			if len(tile.Columns) > 0 && tile.Kind != "table" {
				return fmt.Errorf("%s: `columns:` is only supported on table tiles", where)
			}
			tileCount++
		}
	}
	if tileCount == 0 {
		return errors.New("dashboard has no tiles")
	}
	return nil
}

// ValidateDashboardWatchRefs returns an error if any tile references a
// watch that doesn't exist for this customer. Run this after
// ParseDashboardYAML in any save path.
func (s *Store) ValidateDashboardWatchRefs(orgID OrgID, spec *DashboardSpec) error {
	seen := map[string]bool{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if seen[tile.Watch] {
				continue
			}
			seen[tile.Watch] = true
			if _, err := s.GetCronWatch(orgID, tile.Watch); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return fmt.Errorf("cron %q does not exist", tile.Watch)
				}
				return fmt.Errorf("cron %q: %w", tile.Watch, err)
			}
		}
	}
	return nil
}

// SaveDashboard upserts a dashboard. Callers must have already run
// ParseDashboardYAML and ValidateDashboardWatchRefs against the body.
// userID is recorded as the creator on first write; ignored on update.
func (s *Store) SaveDashboard(orgID OrgID, userID, name, yamlBody string) error {
	return s.scoped(orgID).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		var d Dashboard
		result := tx.Where("name = ?", name).First(&d)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			row := &Dashboard{
				OrgID:    orgID,
				Name:     name,
				YAMLBody: yamlBody,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			return tx.Create(row).Error
		}
		return tx.Model(&d).Update("yaml_body", yamlBody).Error
	})
}

// GetDashboard returns one dashboard by name.
func (s *Store) GetDashboard(orgID OrgID, name string) (*Dashboard, error) {
	var d Dashboard
	err := s.scoped(orgID).Where("name = ?", name).First(&d).Error
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDashboards returns dashboards for a customer ordered by name.
func (s *Store) ListDashboards(orgID OrgID) ([]Dashboard, error) {
	var list []Dashboard
	err := s.scoped(orgID).Order("name").Find(&list).Error
	return list, err
}

// DeleteDashboard removes a dashboard (soft-delete via gorm).
func (s *Store) DeleteDashboard(orgID OrgID, name string) error {
	return s.scoped(orgID).Where("name = ?", name).Delete(&Dashboard{}).Error
}
