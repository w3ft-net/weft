package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// numericTileKinds are the dashboard tiles that draw numbers; a table
// shows text as well.
var numericTileKinds = map[string]bool{"line": true, "bar": true, "sparkline": true, "counter": true, "gauge": true}

// readsNumbers reports whether t draws numbers: a table does only when it
// aggregates over a range, which text can't be.
func readsNumbers(t DashboardTile) bool {
	return numericTileKinds[t.Kind] || (t.Kind == "table" && t.Windowed())
}

// checkValueTypes refuses a save that would put a text cron behind a
// numeric reader: this pipeline reading a text cron with a numeric weft::
// function, or this cron turning text while numeric readers name it.
func (s *Store) checkValueTypes(orgID OrgID, name string, compiled CompiledWatch) error {
	for _, ref := range compiled.NumericRefs {
		if ref == name && compiled.ValueType == ValueText {
			return fmt.Errorf("this cron stores text (# type: text), so it can't read itself with a numeric weft:: function")
		}
	}
	if err := s.refuseTextRefs(orgID, compiled.NumericRefs, name); err != nil {
		return err
	}
	if compiled.ValueType != ValueText {
		return nil
	}
	if existing, err := s.GetCronWatch(orgID, name); err == nil && existing.ValueType == ValueText {
		return nil // already text: its readers were checked when they were saved
	}
	readers, err := s.numericReaders(orgID, name)
	if err != nil {
		return err
	}
	if len(readers) > 0 {
		return fmt.Errorf("can't make %q a text cron while numeric readers use it: %s", name, strings.Join(readers, ", "))
	}
	return nil
}

// refuseTextRefs refuses numeric reads, by literal name, of crons that
// store text. skip is the cron being saved, whose own type is checked by
// the caller.
func (s *Store) refuseTextRefs(orgID OrgID, refs []string, skip string) error {
	for _, ref := range refs {
		if ref == skip {
			continue
		}
		target, err := s.GetCronWatch(orgID, ref)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if target.ValueType == ValueText {
			return fmt.Errorf("cron %q stores text (# type: text); numeric weft functions can't read it, but weft::latest (weft.latest in Lua) can", ref)
		}
	}
	return nil
}

// numericReaders lists what reads cron name as numbers: other crons' and
// /bin scripts' literal numeric weft calls (bash or Lua), and dashboards'
// numeric tiles.
func (s *Store) numericReaders(orgID OrgID, name string) ([]string, error) {
	var out []string
	reads := func(refs []string) bool {
		for _, ref := range refs {
			if ref == name {
				return true
			}
		}
		return false
	}
	fns, err := s.ListShellFunctions(orgID)
	if err != nil {
		return nil, err
	}
	for _, fn := range fns {
		if refs, err := scriptRefs(fn.Language, fn.Source); err == nil && reads(refs) {
			out = append(out, "/bin/"+fn.Name)
		}
	}
	crons, err := s.ListCronWatches(orgID)
	if err != nil {
		return nil, err
	}
	for _, c := range crons {
		if c.Name == name {
			continue
		}
		compiled, err := CompileWatchPipeline(c.Pipeline)
		if err == nil && reads(compiled.NumericRefs) {
			out = append(out, "/cron.d/"+c.Name)
		}
	}
	dashboards, err := s.ListDashboards(orgID)
	if err != nil {
		return nil, err
	}
	for _, d := range dashboards {
		spec, err := ParseDashboardYAML(d.YAMLBody)
		if err != nil {
			continue
		}
		if kinds := numericTiles(spec, name); len(kinds) > 0 {
			out = append(out, fmt.Sprintf("/etc/dashboards/%s.yaml (%s)", d.Name, strings.Join(kinds, ", ")))
		}
	}
	return out, nil
}

// numericTiles returns the kinds of spec's numeric tiles on cron name.
func numericTiles(spec *DashboardSpec, name string) []string {
	seen := map[string]bool{}
	for _, sec := range spec.Sections {
		for _, t := range sec.Tiles {
			if t.Watch == name && readsNumbers(t) {
				seen[t.Kind] = true
			}
		}
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// LatestCronRun returns the points of a cron's newest run that stored any,
// one per tag, in table order: numbers highest first, text by tag. Rows
// with neither a value nor text (a failed run's diagnostic) are skipped.
func (s *Store) LatestCronRun(cronWatchID uint) ([]CronResult, error) {
	var rows []CronResult
	err := s.db.Raw(`
		SELECT * FROM cron_results
		WHERE cron_watch_id = ?
		  AND (value IS NOT NULL OR text_value IS NOT NULL)
		  AND ran_at = (
		    SELECT max(ran_at) FROM cron_results
		    WHERE cron_watch_id = ? AND (value IS NOT NULL OR text_value IS NOT NULL))
		ORDER BY value DESC NULLS LAST, tag ASC`,
		cronWatchID, cronWatchID,
	).Scan(&rows).Error
	return rows, err
}

// checkDashboardTypes refuses a numeric tile on a text cron.
func (s *Store) checkDashboardTypes(orgID OrgID, yamlBody string) error {
	spec, err := ParseDashboardYAML(yamlBody)
	if err != nil {
		return err
	}
	for _, sec := range spec.Sections {
		for _, t := range sec.Tiles {
			if !readsNumbers(t) {
				continue
			}
			w, err := s.GetCronWatch(orgID, t.Watch)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if w.ValueType == ValueText {
				if t.Kind == "table" {
					return fmt.Errorf("cron %q stores text (# type: text), which a table can't aggregate; drop its range and aggregate to show the latest run", t.Watch)
				}
				return fmt.Errorf("cron %q stores text (# type: text); a %s tile draws numbers, so use a table tile", t.Watch, t.Kind)
			}
		}
	}
	return nil
}
