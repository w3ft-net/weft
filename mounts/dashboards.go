package mounts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	warpmounts "github.com/graybeardcorp/warp/pkg/shell/mounts"
	"github.com/w3ft-net/weft/backends/cron"
	dashstore "github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/dashboard"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/warning"
)

// Dashboards is /etc/dashboards: one YAML file per dashboard, kept in st. It is
// an /etc extension (see EtcExtensions), for a build that serves dashboards. A
// deployment may embed the mount to add to it (hosted: version messages).
//
// Every member can read the dashboards; only the configmgr group can change
// them. A dashboard is saved only if it parses, names crons the writer can read,
// and draws numbers only from crons that store numbers.
func Dashboards(cfg Config, st dashstore.Store) *warpmounts.StoreMount {
	d := dashboards{cfg: cfg, st: st}
	return warpmounts.Store("/etc/dashboards", d, warpmounts.Auth(d.authorize), warpmounts.Lang(dashboardLanguage))
}

type dashboards struct {
	cfg Config
	st  dashstore.Store
}

func (d dashboards) authorize(_ context.Context, _ string, want shellcore.Perm) error {
	if want&shellcore.PermWrite != 0 && !d.cfg.allowed(identity.ActionManageEtc) {
		return denyWrite("/etc", "configmgr")
	}
	return nil
}

// dashboardLanguage: the dashboards are YAML.
func dashboardLanguage(rel string) string {
	if strings.HasSuffix(rel, ".yaml") {
		return "yaml"
	}
	return ""
}

// dashboardName is the dashboard a path below /etc/dashboards names: <name>.yaml.
func dashboardName(rel string) (string, bool) {
	name, ok := strings.CutSuffix(rel, ".yaml")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// noSuchDashboard is a read of a dashboard that does not exist.
type noSuchDashboard string

func (e noSuchDashboard) Error() string      { return "/etc/dashboards/" + string(e) + ": no such file" }
func (noSuchDashboard) Is(target error) bool { return target == fs.ErrNotExist }

func (d dashboards) List(context.Context) ([]warpmounts.Entry, error) {
	list, err := d.st.List(d.cfg.partition())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, dash := range list {
		if dash.CreatedBy != "" {
			ids = append(ids, dash.CreatedBy)
		}
	}
	owners := d.cfg.OwnerNames(ids)
	entries := make([]warpmounts.Entry, len(list))
	for i, dash := range list {
		entries[i] = warpmounts.Entry{
			Name:    dash.Name + ".yaml",
			Size:    int64(len(dash.YAMLBody)),
			ModTime: dash.UpdatedAt,
			Owner:   owners[dash.CreatedBy],
		}
	}
	return entries, nil
}

func (d dashboards) Get(_ context.Context, rel string) ([]byte, error) {
	if name, ok := dashboardName(rel); ok {
		if dash, err := d.st.Get(d.cfg.partition(), name); err == nil {
			return []byte(dash.YAMLBody), nil
		}
	}
	return nil, noSuchDashboard(rel)
}

func (d dashboards) put(rel string, data []byte) error {
	name, ok := dashboardName(rel)
	if !ok {
		return fmt.Errorf("/etc/dashboards/%s: read-only", rel)
	}
	body := string(data)
	spec, err := dashboard.Parse(body)
	if err != nil {
		return fmt.Errorf("/etc/dashboards/%s: %w", rel, err)
	}
	readable, _, err := d.cfg.Readable()
	if err != nil {
		return fmt.Errorf("/etc/dashboards/%s: %w", rel, err)
	}
	if err := d.checkCrons(spec, readable); err != nil {
		return fmt.Errorf("/etc/dashboards/%s: %w", rel, err)
	}
	return d.st.Save(d.cfg.partition(), d.cfg.UserID, name, body)
}

func (d dashboards) Put(ctx context.Context, rel string, data []byte) error {
	err := d.put(rel, data)
	if _, ok := dashboardName(rel); ok {
		d.cfg.JournalWrite("etc/dashboards/"+rel, err, func() ([]byte, error) { return d.Get(ctx, rel) })
	}
	return err
}

func (d dashboards) Delete(_ context.Context, rel string) error {
	name, ok := dashboardName(rel)
	if !ok {
		return fmt.Errorf("/etc/dashboards/%s: operation not permitted", rel)
	}
	err := d.st.Delete(d.cfg.partition(), name)
	d.cfg.JournalDelete("etc/dashboards/"+rel, err)
	return err
}

// cronAt is the cron a tile names, <namespace>/<name>, or an owner-level cron's
// bare name.
func (c Config) cronAt(path string) (*cron.Watch, error) {
	ns, name, found := strings.Cut(path, "/")
	var home uint
	if found {
		n, ok, err := c.cronAuthor().Sources.GetNamespace(c.partition(), ns)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, cron.ErrNotFound
		}
		home = n.ID
	} else {
		name = path
	}
	return c.cronAuthor().Crons.GetWatch(c.partition(), home, name)
}

// checkCrons refuses a dashboard whose tiles name a cron that doesn't exist or
// that the writer can't read (a tile names its cron as <namespace>/<name>, and
// the writer must be able to read that namespace; a cron they can't read reads
// as one that doesn't exist), or that draw numbers from a cron that stores text.
func (d dashboards) checkCrons(spec *dashboard.Spec, readable []string) error {
	seen := map[string]bool{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if seen[tile.Watch] {
				continue
			}
			seen[tile.Watch] = true
			home, _, _ := strings.Cut(tile.Watch, "/")
			_, err := d.cfg.cronAt(tile.Watch)
			if err == nil && !slices.Contains(readable, home) {
				err = cron.ErrNotFound
			}
			if err != nil {
				if errors.Is(err, cron.ErrNotFound) {
					return fmt.Errorf("cron %q does not exist", tile.Watch)
				}
				return fmt.Errorf("cron %q: %w", tile.Watch, err)
			}
		}
	}
	// A text cron has no numbers for a numeric tile to draw.
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if !dashboard.ReadsNumbers(tile) {
				continue
			}
			if w, err := d.cfg.cronAt(tile.Watch); err == nil && w.Compiled.ValueType == cron.ValueText {
				if tile.Kind == "table" {
					return fmt.Errorf("cron %q stores text (# type: text), which a table can't aggregate; drop its range and aggregate to show the latest run", tile.Watch)
				}
				return fmt.Errorf("cron %q stores text (# type: text); a %s tile draws numbers, so use a table tile", tile.Watch, tile.Kind)
			}
		}
	}
	return nil
}

// DashboardCronRenamed is Config.CronRenamed over st: a warning for the
// dashboards that still name an owner-level cron that has just been moved, since
// a tile naming the old path no longer finds it.
func DashboardCronRenamed(st dashstore.Store) func(c Config, name, newRel string) error {
	return func(c Config, name, newRel string) error {
		list, err := st.List(c.partition())
		if err != nil {
			return nil
		}
		var refs []string
		for _, d := range list {
			spec, err := dashboard.Parse(d.YAMLBody)
			if err != nil {
				continue
			}
		tiles:
			for _, sec := range spec.Sections {
				for _, t := range sec.Tiles {
					if t.Watch == name {
						refs = append(refs, d.Name)
						break tiles
					}
				}
			}
		}
		if len(refs) == 0 {
			return nil
		}
		return warning.New("moved /cron.d/%s to /cron.d/%s, but dashboards %s still name it %q; point their tiles at %q", name, newRel, strings.Join(refs, ", "), name, newRel)
	}
}
